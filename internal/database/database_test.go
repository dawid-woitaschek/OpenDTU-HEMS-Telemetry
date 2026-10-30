package database

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"opendtu-hems-telemetry/internal/auth"
	"opendtu-hems-telemetry/internal/telemetry"
)

var testTime = time.Date(2026, 10, 30, 14, 5, 0, 0, time.UTC)

func testCipher(t *testing.T, value byte) *auth.Cipher {
	t.Helper()
	c, err := auth.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "telemetry.db"), testCipher(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createTestSource(t *testing.T, s *Store, name string) (Source, string) {
	t.Helper()
	source, err := s.CreateSource(context.Background(), name, testTime)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := s.ShowToken(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return source, token
}

func TestPersistenceAssociationAndRevocation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	s, err := Open(path, testCipher(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	source, token := createTestSource(t, s, "Schwägerin")
	var encrypted []byte
	if err := s.db.QueryRow("SELECT token_encrypted FROM telemetry_sources WHERE id = ?", source.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(token)) {
		t.Fatal("stored plaintext token")
	}
	raw := []byte(" \n{\"schema_version\": 1, \"source_name\": \"attacker\", \"heap\": {\"psram\": {\"raw_future\": [1,2]}}, \"modbus\": {\"anything\": {\"registers\": {\"123\": 456}}}}\n")
	m, err := telemetry.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u, duplicate, err := s.Accept(ctx, token, raw, m, testTime)
	if err != nil || duplicate {
		t.Fatalf("accept: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if incorrect, err := Open(path, testCipher(t, 2)); err == nil {
		incorrect.Close()
		t.Fatal("wrong master key accepted")
	}
	s, err = Open(path, testCipher(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Upload(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceID != source.ID || got.SourceName != "Schwägerin" || !bytes.Equal(got.Raw, raw) || got.ReceivedAt != Timestamp(testTime) {
		t.Fatalf("incorrect persisted upload: %+v", got)
	}
	if err := s.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}
	_, again, err := s.ShowToken(ctx, source.ID)
	if err != nil || again != token {
		t.Fatal("token not recoverable after restart")
	}
	if err := s.RevokeSource(ctx, source.ID, testTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked token authenticated")
	}
	if _, _, err := s.Accept(ctx, token, raw, m, testTime); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked token ingested")
	}
	if _, err := s.Upload(ctx, u.ID); err != nil {
		t.Fatal("revocation removed existing telemetry")
	}
	var hash []byte
	if err := s.db.QueryRow("SELECT token_encrypted, token_hash FROM telemetry_sources WHERE id = ?", source.ID).Scan(&encrypted, &hash); err != nil {
		t.Fatal(err)
	}
	if encrypted != nil || hash != nil {
		t.Fatal("revoked credential was retained")
	}
}

func TestDuplicateScopeAndLastSeen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, tokenA := createTestSource(t, s, "A")
	_, tokenB := createTestSource(t, s, "B")
	for _, tc := range []struct {
		raw string
	}{
		{`{"schema_version":1,"boot_id":"boot-a","telemetry_sequence":1}`},
		{`{"schema_version":1,"boot_id":"boot-b","telemetry_sequence":1}`},
		{`{"schema_version":1,"upload_id":"upload"}`},
		{`{"schema_version":1,"boot_id":"boot-a","checkpoint_sequence":1}`},
	} {
		m, _ := telemetry.Parse([]byte(tc.raw))
		first, dup, err := s.Accept(ctx, tokenA, []byte(tc.raw), m, testTime)
		if err != nil || dup {
			t.Fatalf("first upload rejected: %v", err)
		}
		second, dup, err := s.Accept(ctx, tokenA, []byte(tc.raw), m, testTime.Add(time.Hour))
		if err != nil || !dup || first.ID != second.ID || first.ReceivedAt != second.ReceivedAt {
			t.Fatalf("duplicate not idempotent: %v", err)
		}
		if _, dup, err := s.Accept(ctx, tokenB, []byte(tc.raw), m, testTime); err != nil || dup {
			t.Fatal("cross-source duplicate")
		}
	}
	raw := []byte(`{"schema_version":1,"sequence":1}`)
	m, _ := telemetry.Parse(raw)
	for range 2 {
		if _, dup, err := s.Accept(ctx, tokenA, raw, m, testTime); err != nil || dup {
			t.Fatal("missing boot ID prevented ingest")
		}
	}
	uploads, err := s.Uploads(ctx, Filter{SourceID: a.ID})
	if err != nil || len(uploads) != 6 {
		t.Fatalf("wrong upload count: %d, %v", len(uploads), err)
	}
	sources, err := s.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range sources {
		if src.ID == a.ID && src.LastSeen != Timestamp(testTime.Add(time.Hour)) {
			t.Fatal("last seen went backwards")
		}
	}
}

func TestConcurrentRetries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, token := createTestSource(t, s, "A")
	raw := []byte(`{"schema_version":1,"boot_id":"b","sequence":1}`)
	m, _ := telemetry.Parse(raw)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := s.Accept(ctx, token, raw, m, testTime); err != nil {
				t.Errorf("concurrent ingestion: %v", err)
			}
		})
	}
	wg.Wait()
	rows, err := s.Uploads(ctx, Filter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("retries stored %d rows: %v", len(rows), err)
	}
}
