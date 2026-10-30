package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opendtu-hems-telemetry/internal/auth"
)

// A frozen v1 schema is intentionally independent of the migration code.
func legacyDatabase(t *testing.T) (string, string, string, map[int64][]byte, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := os.ReadFile("testdata/schema_v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	active, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := testCipher(t, 1).Encrypt(active, 17)
	if err != nil {
		t.Fatal(err)
	}
	created, seen, removed := Timestamp(testTime), Timestamp(testTime.Add(time.Hour)), Timestamp(testTime.Add(2*time.Hour))
	if _, err := db.Exec(`INSERT INTO telemetry_sources(id, name, token_encrypted, token_hash, created_at, last_seen_at) VALUES (17, 'Schwägerin', ?, ?, ?, ?)`, encrypted, auth.Digest(active), created, seen); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO telemetry_sources(id, name, created_at, last_seen_at, revoked_at) VALUES (41, 'Testgerät B', ?, ?, ?)`, created, seen, removed); err != nil {
		t.Fatal(err)
	}
	raw := map[int64][]byte{
		105: []byte(" \n{\"schema_version\":1,\"checkpoint_sequence\":77,\"heap\":{\"psram\":{\"future\":[1,2]}}}\n"),
		206: []byte(" {\"schema_version\":1,\"modbus\":{\"unknown\":{\"registers\":{\"999\":[1,2]}}}}\n"),
	}
	for uploadID, sourceID := range map[int64]int64{105: 17, 206: 41} {
		_, err := db.Exec(`INSERT INTO telemetry_uploads(id, source_id, received_at, schema_version, boot_id, upload_id, uptime, firmware, build_identity, reset_reason, telemetry_sequence, checkpoint_sequence, dedupe_key, summary_json, raw_json) VALUES (?, ?, ?, 1, 'legacy-boot', '', '99', 'legacy-fw', 'abc', 'POWERON', '42', '77', NULL, '{"psram_free":"123"}', ?)`, uploadID, sourceID, seen, raw[uploadID])
		if err != nil {
			t.Fatal(err)
		}
	}
	return path, active, deleted, raw, encrypted
}

func TestMigrationV1ToV3(t *testing.T) {
	path, active, deleted, raw, encrypted := legacyDatabase(t)
	s, err := Open(path, testCipher(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatalf("schema version: %d, %v", version, err)
	}
	var tableSQL string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'telemetry_sources'").Scan(&tableSQL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tableSQL, "revoked_at") {
		t.Fatal("source revocation state survived migration")
	}
	sources, err := s.Sources(ctx)
	if err != nil || len(sources) != 2 {
		t.Fatalf("migration changed source count: %v", err)
	}
	for _, source := range sources {
		if source.CreatedAt != Timestamp(testTime) || source.LastSeen != Timestamp(testTime.Add(time.Hour)) {
			t.Fatal("migration changed source timestamps")
		}
		switch source.ID {
		case 17:
			if source.Name != "Schwägerin" || !source.HasToken || source.TokenCreatedAt != source.CreatedAt || source.TokenDeletedAt != "" {
				t.Fatal("active source not preserved")
			}
		case 41:
			if source.Name != "Testgerät B" || source.HasToken || source.TokenCreatedAt != "" || source.TokenDeletedAt != Timestamp(testTime.Add(2*time.Hour)) {
				t.Fatal("revoked v1 source did not become a normal tokenless source")
			}
		default:
			t.Fatal("source ID changed")
		}
	}
	_, shown, err := s.ShowToken(ctx, 17)
	if err != nil || shown != active {
		t.Fatal("active token no longer decrypts with original source ID")
	}
	var ciphertext, digest []byte
	if err := s.db.QueryRow("SELECT token_encrypted, token_hash FROM telemetry_sources WHERE id = 17").Scan(&ciphertext, &digest); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ciphertext, encrypted) || !bytes.Equal(digest, auth.Digest(active)) {
		t.Fatal("migration rewrote credentials")
	}
	if err := s.Authenticate(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, deleted); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted legacy token authenticated")
	}
	if _, _, err := s.ShowToken(ctx, 41); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("tokenless legacy source showed a token")
	}
	for uploadID, sourceID := range map[int64]int64{105: 17, 206: 41} {
		projected, err := s.Dashboard(ctx, Filter{SourceID: sourceID}, true, testTime.Add(time.Hour))
		if err != nil || projected.Current == nil || projected.Current.Point.ID != uploadID || projected.Current.Point.TimeBasis != "collector_receive_fallback" {
			t.Fatalf("legacy projection missing: %+v, %v", projected, err)
		}
		u, err := s.Upload(ctx, uploadID)
		if err != nil || u.SourceID != sourceID || !bytes.Equal(u.Raw, raw[uploadID]) || u.Metadata.Firmware != "legacy-fw" || u.Metadata.CheckpointSequence != "77" || u.Metadata.Summary.PSRAMFree != "123" {
			t.Fatalf("migration changed upload %d: %v", uploadID, err)
		}
	}
	checks, err := s.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if checks.Next() {
		t.Fatal("migration broke a foreign key")
	}
	checks.Close()
	if err := s.CreateToken(ctx, 41, testTime.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, replacement, err := s.ShowToken(ctx, 41)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, deleted); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("replacement resurrected deleted v1 token")
	}
	if u, err := s.Upload(ctx, 206); err != nil || u.SourceID != 41 {
		t.Fatal("replacement lost legacy history")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, testCipher(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Authenticate(ctx, replacement); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRollsBackWhenKeyVerificationFails(t *testing.T) {
	path, _, _, _, encrypted := legacyDatabase(t)
	if s, err := Open(path, testCipher(t, 2)); err == nil {
		s.Close()
		t.Fatal("incorrect key accepted")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("failed migration changed schema version")
	}
	var revoked string
	if err := db.QueryRow("SELECT revoked_at FROM telemetry_sources WHERE id = 41").Scan(&revoked); err != nil || revoked != Timestamp(testTime.Add(2*time.Hour)) {
		t.Fatal("migration column rename was not rolled back")
	}
	var actual []byte
	if err := db.QueryRow("SELECT token_encrypted FROM telemetry_sources WHERE id = 17").Scan(&actual); err != nil || !bytes.Equal(actual, encrypted) {
		t.Fatal("failed migration changed credentials")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM telemetry_uploads").Scan(&count); err != nil || count != 2 {
		t.Fatal("failed migration changed uploads")
	}
}

func TestMigrationDoesNotResurrectRevokedV1Credentials(t *testing.T) {
	path, _, deleted, _, _ := legacyDatabase(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A v1 row's revoked_at was authoritative even if credential bytes remained.
	encrypted, err := testCipher(t, 1).Encrypt(deleted, 41)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE telemetry_sources SET token_encrypted = ?, token_hash = ? WHERE id = 41", encrypted, auth.Digest(deleted)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path, testCipher(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Authenticate(context.Background(), deleted); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("migration resurrected revoked v1 credentials")
	}
	if _, _, err := s.ShowToken(context.Background(), 41); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("migration retained revoked token")
	}
	if err := s.CreateToken(context.Background(), 41, testTime); err != nil {
		t.Fatal("migrated source cannot create a new token")
	}
}
