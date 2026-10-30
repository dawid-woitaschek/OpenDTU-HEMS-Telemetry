package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"opendtu-hems-telemetry/internal/database"
)

func acknowledgement(t *testing.T, w *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	requireStatus(t, w, 200)
	var ack map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	if string(ack["accepted"]) != "true" {
		t.Fatal("upload was not acknowledged")
	}
	return ack
}

func TestCheckpointAcknowledgement(t *testing.T) {
	for _, tc := range []struct{ name, fields, sequence string }{
		{"integer", `,"checkpoint_sequence":1487`, "1487"},
		{"zero", `,"checkpoint_sequence":0`, "0"},
		{"uint64", `,"checkpoint_sequence":18446744073709551615`, "18446744073709551615"},
		{"nested", `,"diagnostics":{"checkpoint_sequence":1487}`, "1487"},
		{"absent", "", ""},
		{"null", `,"checkpoint_sequence":null`, ""},
		{"string", `,"checkpoint_sequence":"1487"`, ""},
		{"negative", `,"checkpoint_sequence":-1`, ""},
		{"fraction", `,"checkpoint_sequence":1.5`, ""},
		{"exponent", `,"checkpoint_sequence":1e3`, ""},
		{"boolean", `,"checkpoint_sequence":true`, ""},
		{"object", `,"checkpoint_sequence":{"value":1487}`, ""},
		{"overflow", `,"checkpoint_sequence":18446744073709551616`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			raw := `{"schema_version":1` + tc.fields + `}`
			ack := acknowledgement(t, f.request("POST", "/api/v1/telemetry", raw, f.token, "application/json"))
			if string(ack["accepted_checkpoint_sequence"]) != tc.sequence {
				t.Fatal("wrong checkpoint acknowledgement")
			}
			rows, err := f.store.Uploads(context.Background(), database.Filter{})
			if err != nil || len(rows) != 1 {
				t.Fatal("acknowledged upload not persisted")
			}
			u, err := f.store.Upload(context.Background(), rows[0].ID)
			if err != nil || string(u.Raw) != raw {
				t.Fatal("acknowledged payload changed")
			}
			var received string
			if err := json.Unmarshal(ack["received_at"], &received); err != nil || received != u.ReceivedAt {
				t.Fatal("acknowledgement does not match committed upload")
			}
		})
	}
}

func TestDuplicateCheckpointAcknowledgementUsesPersistedPayload(t *testing.T) {
	for _, tc := range []struct{ name, original, retry, sequence string }{
		{"identical", `,"checkpoint_sequence":1487`, `,"checkpoint_sequence":1487`, "1487"},
		{"changed checkpoint", `,"checkpoint_sequence":1487`, `,"checkpoint_sequence":1488`, "1487"},
		{"original absent", "", `,"checkpoint_sequence":1487`, ""},
		{"original invalid", `,"checkpoint_sequence":"1487"`, `,"checkpoint_sequence":1487`, ""},
		{"retry absent", `,"checkpoint_sequence":1487`, "", ""},
		{"retry invalid", `,"checkpoint_sequence":1487`, `,"checkpoint_sequence":"1487"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			original := `{"schema_version":1,"upload_id":"same"` + tc.original + `}`
			retry := `{"schema_version":1,"upload_id":"same"` + tc.retry + `}`
			first := acknowledgement(t, f.request("POST", "/api/v1/telemetry", original, f.token, "application/json"))
			second := acknowledgement(t, f.request("POST", "/api/v1/telemetry", retry, f.token, "application/json"))
			if string(second["accepted_checkpoint_sequence"]) != tc.sequence || string(second["duplicate"]) != "true" || string(first["received_at"]) != string(second["received_at"]) {
				t.Fatal("duplicate acknowledged unpersisted data or changed receive time")
			}
			rows, err := f.store.Uploads(context.Background(), database.Filter{})
			if err != nil || len(rows) != 1 {
				t.Fatal("duplicate was stored")
			}
			u, err := f.store.Upload(context.Background(), rows[0].ID)
			if err != nil || string(u.Raw) != original {
				t.Fatal("retry replaced the authoritative payload")
			}
		})
	}
}

func TestFailedPersistenceDoesNotAcknowledgeCheckpoint(t *testing.T) {
	f := setup(t)
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Force a real SQLite write failure without replacing the storage layer.
	_, err = db.Exec(`CREATE TRIGGER reject_upload BEFORE INSERT ON telemetry_uploads BEGIN SELECT RAISE(ABORT, 'test write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	w := f.request("POST", "/api/v1/telemetry", `{"schema_version":1,"checkpoint_sequence":1487}`, f.token, "application/json")
	requireStatus(t, w, 500)
	if strings.Contains(w.Body.String(), "accepted_checkpoint_sequence") || strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatal("failed write acknowledged")
	}
	rows, err := f.store.Uploads(context.Background(), database.Filter{})
	if err != nil || len(rows) != 0 {
		t.Fatal("failed write was persisted")
	}
	sources, err := f.store.Sources(context.Background())
	if err != nil || len(sources) != 1 || sources[0].LastSeen != "" {
		t.Fatal("failed ingestion did not roll back last-seen update")
	}
}
