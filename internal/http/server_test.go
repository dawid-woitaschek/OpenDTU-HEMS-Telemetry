package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"opendtu-hems-telemetry/internal/auth"
	"opendtu-hems-telemetry/internal/database"
	"opendtu-hems-telemetry/internal/telemetry"
)

type fixture struct {
	t       *testing.T
	store   *database.Store
	handler http.Handler
	source  database.Source
	token   string
	logs    bytes.Buffer
	dbPath  string
}

var fixedTime = time.Date(2026, 10, 30, 14, 5, 0, 0, time.UTC)

func setup(t *testing.T) *fixture {
	t.Helper()
	cipher, err := auth.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := database.Open(dbPath, cipher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, store: store, dbPath: dbPath}
	f.handler, err = New(store, "admin", "test-password", log.New(&f.logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	f.source, err = store.CreateSource(context.Background(), "Schwägerin", fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateToken(context.Background(), f.source.ID, fixedTime); err != nil {
		t.Fatal(err)
	}
	_, f.token, err = store.ShowToken(context.Background(), f.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) request(method, path, body, credentials, contentType string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if credentials == "admin" {
		r.SetBasicAuth("admin", "test-password")
	} else if credentials != "" {
		r.Header.Set("Authorization", "Bearer "+credentials)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func requireStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
	}
}

func TestIngestionAndSourceAssociation(t *testing.T) {
	f := setup(t)
	raw := " \n{\"schema_version\":1,\"source_id\":999,\"source_name\":\"spoof\",\"system\":{\"uptime_seconds\":83640,\"firmware_version\":\"test\"},\"heap\":{\"psram\":{\"free_bytes\":4321}},\"future\":{\"raw\":[1,2]}}\n"
	w := f.request("POST", "/api/v1/telemetry", raw, f.token, "application/json; charset=utf-8")
	requireStatus(t, w, http.StatusOK)
	var ack struct {
		Accepted   bool   `json:"accepted"`
		ReceivedAt string `json:"received_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil || !ack.Accepted {
		t.Fatalf("bad acknowledgement: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, ack.ReceivedAt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), f.token) {
		t.Fatal("acknowledgement leaked credential")
	}
	rows, err := f.store.Uploads(context.Background(), database.Filter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("upload not persisted: %v", err)
	}
	u, err := f.store.Upload(context.Background(), rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(u.Raw) != raw || u.SourceID != f.source.ID || u.SourceName != "Schwägerin" || u.Metadata.Summary.PSRAMFree != "4321" {
		t.Fatal("source association or raw payload changed")
	}
	for _, path := range []string{"/sources", "/uploads", fmt.Sprintf("/uploads/%d", u.ID), "/downloads"} {
		requireStatus(t, f.request("GET", path, "", "admin", ""), http.StatusOK)
	}
	w = f.request("GET", fmt.Sprintf("/uploads/%d/download", u.ID), "", "admin", "")
	requireStatus(t, w, http.StatusOK)
	if w.Header().Get("Content-Disposition") != fmt.Sprintf(`attachment; filename="telemetry-%d.json"`, u.ID) {
		t.Fatal("incorrect snapshot Content-Disposition")
	}
	if w.Body.String() != raw {
		t.Fatal("single download changed payload")
	}
	if strings.Contains(f.logs.String(), f.token) || strings.Contains(f.logs.String(), "test-password") {
		t.Fatal("credential logged")
	}
}

func TestIngestionValidation(t *testing.T) {
	f := setup(t)
	other, _ := auth.GenerateToken()
	for _, tc := range []struct {
		name, method, body, token, contentType string
		status                                 int
	}{
		{"missing auth", "POST", `{"schema_version":1}`, "", "application/json", 401},
		{"unknown token", "POST", `{"schema_version":1}`, other, "application/json", 401},
		{"admin is not device", "POST", `{"schema_version":1}`, "admin", "application/json", 401},
		{"malformed", "POST", "{", f.token, "application/json", 400},
		{"trailing object", "POST", `{"schema_version":1} {}`, f.token, "application/json", 400},
		{"missing schema", "POST", `{}`, f.token, "application/json", 400},
		{"unsupported schema", "POST", `{"schema_version":2}`, f.token, "application/json", 400},
		{"wrong content type", "POST", `{"schema_version":1}`, f.token, "text/plain", 415},
		{"missing content type", "POST", `{"schema_version":1}`, f.token, "", 415},
		{"wrong method", "GET", "", f.token, "", 405},
		{"oversized", "POST", strings.Repeat(" ", int(MaxTelemetryBytes)) + `{"schema_version":1}`, f.token, "application/json", 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireStatus(t, f.request(tc.method, "/api/v1/telemetry", tc.body, tc.token, tc.contentType), tc.status)
		})
	}
	rows, err := f.store.Uploads(context.Background(), database.Filter{})
	if err != nil || len(rows) != 0 {
		t.Fatal("rejected uploads persisted")
	}
}

func TestBodyLimitWithoutContentLength(t *testing.T) {
	f := setup(t)
	payload := `{"schema_version":1}`
	boundary := payload + strings.Repeat(" ", int(MaxTelemetryBytes)-len(payload))
	for _, tc := range []struct {
		body   string
		status int
	}{{boundary, 200}, {boundary + " ", 413}} {
		r := httptest.NewRequest("POST", "/api/v1/telemetry", strings.NewReader(tc.body))
		r.ContentLength = -1
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+f.token)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		requireStatus(t, w, tc.status)
	}
	r := httptest.NewRequest("POST", "/api/v1/telemetry", strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Encoding", "gzip")
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	requireStatus(t, w, 415)
}

func TestHealthAndAdministrativeProtection(t *testing.T) {
	f := setup(t)
	requireStatus(t, f.request("GET", "/health", "", "", ""), 200)
	for _, path := range []string{"/", "/sources", "/uploads", "/uploads/1", "/uploads/1/download", "/downloads", "/downloads/export.zip"} {
		requireStatus(t, f.request("GET", path, "", "", ""), 401)
		requireStatus(t, f.request("GET", path, "", f.token, ""), 401)
	}
	for _, path := range []string{"/sources", "/sources/1/token", "/sources/1/token/create", "/sources/1/token/delete"} {
		requireStatus(t, f.request("POST", path, "", "", "application/x-www-form-urlencoded"), 401)
		requireStatus(t, f.request("POST", path, "csrf=invalid", "admin", "application/x-www-form-urlencoded"), 403)
	}
	r := httptest.NewRequest("GET", "/sources", nil)
	r.SetBasicAuth("admin", "wrong")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	requireStatus(t, w, 401)
	requireStatus(t, f.request("GET", "/etc/passwd", "", "admin", ""), 404)
}

func (f *fixture) csrf() string {
	w := f.request("GET", "/sources", "", "admin", "")
	requireStatus(f.t, w, 200)
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		f.t.Fatal("missing CSRF form token")
	}
	return match[1]
}

func TestSourceTokenLifecycleThroughUI(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	csrf := f.csrf()
	form := url.Values{"csrf": {csrf}, "name": {"Testgerät B"}}
	requireStatus(t, f.request("POST", "/sources", form.Encode(), "admin", "application/x-www-form-urlencoded"), 303)
	sources, err := f.store.Sources(ctx)
	if err != nil || len(sources) != 2 {
		t.Fatal("source not created")
	}
	created := sources[0]
	if created.HasToken {
		t.Fatal("new source should have a Create token action")
	}
	form.Del("name")
	sourceURL := fmt.Sprintf("/sources/%d/token", created.ID)
	assertActions := func(active bool) {
		t.Helper()
		w := f.request("GET", "/sources", "", "admin", "")
		requireStatus(t, w, 200)
		body := w.Body.String()
		if strings.Contains(body, "(revoked)") {
			t.Fatal("source marked revoked")
		}
		if strings.Contains(body, `action="`+sourceURL+`/create"`) == active || strings.Contains(body, `action="`+sourceURL+`/delete"`) != active {
			t.Fatal("incorrect source/token actions")
		}
	}
	assertActions(false)
	requireStatus(t, f.request("POST", sourceURL, form.Encode(), "admin", "application/x-www-form-urlencoded"), 404)
	requireStatus(t, f.request("POST", sourceURL+"/create", form.Encode(), "admin", "application/x-www-form-urlencoded"), 303)
	assertActions(true)
	w := f.request("POST", sourceURL, form.Encode(), "admin", "application/x-www-form-urlencoded")
	requireStatus(t, w, 200)
	_, token, err := f.store.ShowToken(ctx, created.ID)
	if err != nil || !strings.Contains(w.Body.String(), token) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token not shown securely")
	}
	requireStatus(t, f.request("POST", sourceURL+"/create", form.Encode(), "admin", "application/x-www-form-urlencoded"), 409)
	original := `{"schema_version":1,"boot_id":"b","sequence":1,"checkpoint_sequence":77}`
	requireStatus(t, f.request("POST", "/api/v1/telemetry", original, token, "application/json"), 200)
	requireStatus(t, f.request("POST", sourceURL+"/delete", form.Encode(), "admin", "application/x-www-form-urlencoded"), 303)
	assertActions(false)
	requireStatus(t, f.request("POST", "/api/v1/telemetry", original, token, "application/json"), 401)
	requireStatus(t, f.request("POST", sourceURL, form.Encode(), "admin", "application/x-www-form-urlencoded"), 404)
	requireStatus(t, f.request("POST", sourceURL+"/create", form.Encode(), "admin", "application/x-www-form-urlencoded"), 303)
	assertActions(true)
	_, replacement, err := f.store.ShowToken(ctx, created.ID)
	if err != nil || replacement == token {
		t.Fatal("credential not replaced")
	}
	requireStatus(t, f.request("POST", "/api/v1/telemetry", `{"schema_version":1,"boot_id":"b","sequence":2}`, replacement, "application/json"), 200)
	requireStatus(t, f.request("POST", "/api/v1/telemetry", original, token, "application/json"), 401)
	uploads, err := f.store.Uploads(ctx, database.Filter{SourceID: created.ID})
	if err != nil || len(uploads) != 2 {
		t.Fatal("token replacement changed source history")
	}
	for _, upload := range uploads {
		if upload.SourceID != created.ID || upload.SourceName != created.Name {
			t.Fatal("history changed ownership")
		}
	}
	history := f.request("GET", fmt.Sprintf("/uploads/%d/download", uploads[1].ID), "", "admin", "")
	requireStatus(t, history, 200)
	if history.Body.String() != original {
		t.Fatal("historical payload changed")
	}
	sources, err = f.store.Sources(ctx)
	if err != nil || len(sources) != 2 {
		t.Fatal("token replacement created a second source")
	}
	for _, secret := range []string{token, replacement} {
		if strings.Contains(f.logs.String(), secret) {
			t.Fatal("credential logged")
		}
	}
}

type callbackReader struct {
	read io.Reader
	once func()
}

func (r *callbackReader) Read(p []byte) (int, error) {
	if r.once != nil {
		fn := r.once
		r.once = nil
		fn()
	}
	return r.read.Read(p)
}

func TestRevocationWhileReadingBody(t *testing.T) {
	f := setup(t)
	body := &callbackReader{read: strings.NewReader(`{"schema_version":1}`), once: func() {
		if err := f.store.DeleteToken(context.Background(), f.source.ID, fixedTime); err != nil {
			t.Fatal(err)
		}
		if err := f.store.CreateToken(context.Background(), f.source.ID, fixedTime); err != nil {
			t.Fatal(err)
		}
	}}
	r := httptest.NewRequest("POST", "/api/v1/telemetry", body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	requireStatus(t, w, 401)
}

func TestDuplicateAcknowledgement(t *testing.T) {
	f := setup(t)
	raw := `{"schema_version":1,"boot_id":"b","telemetry_sequence":42}`
	a := f.request("POST", "/api/v1/telemetry", raw, f.token, "application/json")
	b := f.request("POST", "/api/v1/telemetry", raw, f.token, "application/json")
	requireStatus(t, a, 200)
	requireStatus(t, b, 200)
	var first, second map[string]any
	json.Unmarshal(a.Body.Bytes(), &first)
	json.Unmarshal(b.Body.Bytes(), &second)
	if second["duplicate"] != true || first["received_at"] != second["received_at"] {
		t.Fatal("retry acknowledgement is not idempotent")
	}
	rows, err := f.store.Uploads(context.Background(), database.Filter{})
	if err != nil || len(rows) != 1 {
		t.Fatal("retry was stored twice")
	}
}

func TestZIPExportFiltersAndExactBytes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	other, err := f.store.CreateSource(ctx, "Other", fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateToken(ctx, other.ID, fixedTime); err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := f.store.ShowToken(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(" \n{\"schema_version\": 1, \"unicode\": \"Schwägerin\", \"unknown\": [1, 2]}\n")
	m, err := telemetry.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		at    time.Time
	}{
		{f.token, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)},
		{f.token, time.Date(2026, 10, 30, 23, 59, 59, 999999999, time.UTC)},
		{f.token, time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)},
		{otherToken, fixedTime},
	} {
		if _, _, err := f.store.Accept(ctx, tc.token, raw, m, tc.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.DeleteToken(ctx, f.source.ID, fixedTime); err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", fmt.Sprintf("/downloads/export.zip?source_id=%d&from=2026-10-30&to=2026-10-30", f.source.ID), "", "admin", "")
	requireStatus(t, w, 200)
	if w.Header().Get("Content-Disposition") != `attachment; filename="telemetry.zip"` {
		t.Fatal("incorrect ZIP Content-Disposition")
	}
	z, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(z.File) != 4 {
		t.Fatalf("wrong number of export entries: %d", len(z.File))
	}
	for _, entry := range z.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(entry.Name, "metadata/") {
			var meta map[string]any
			if err := json.Unmarshal(b, &meta); err != nil || meta["source_name"] != "Schwägerin" {
				t.Fatal("export metadata missing source name")
			}
		} else if !bytes.Equal(b, raw) {
			t.Fatal("ZIP changed original payload")
		}
	}
	w = f.request("GET", "/downloads/export.zip", "", "admin", "")
	z, err = zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil || len(z.File) != 8 {
		t.Fatalf("full export: %v", err)
	}
	for _, query := range []string{"source_id=bad", "from=2026-10-31&to=2026-10-30", "to=wrong", "from=wrong"} {
		requireStatus(t, f.request("GET", "/downloads/export.zip?"+query, "", "admin", ""), 400)
	}
}
