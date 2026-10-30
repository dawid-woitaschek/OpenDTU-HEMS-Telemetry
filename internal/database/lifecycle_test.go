package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"opendtu-hems-telemetry/internal/telemetry"
)

func TestSourceTokenLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	source, err := s.CreateSource(ctx, "Schwägerin", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if source.HasToken {
		t.Fatal("new source unexpectedly has a token")
	}
	if _, _, err := s.ShowToken(ctx, source.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("tokenless source showed a token")
	}
	created := testTime.Add(time.Minute)
	if err := s.CreateToken(ctx, source.ID, created); err != nil {
		t.Fatal(err)
	}
	_, oldToken, err := s.ShowToken(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, oldToken); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateToken(ctx, source.ID, created); !errors.Is(err, ErrTokenExists) {
		t.Fatal("active token was replaceable without deletion")
	}
	_, unchanged, err := s.ShowToken(ctx, source.ID)
	if err != nil || unchanged != oldToken {
		t.Fatal("second creation replaced the active token")
	}
	raw := []byte(`{"schema_version":1,"boot_id":"b","telemetry_sequence":1,"checkpoint_sequence":77}`)
	m, err := telemetry.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Accept(ctx, oldToken, raw, m, created)
	if err != nil {
		t.Fatal(err)
	}
	deleted := created.Add(time.Minute)
	if err := s.DeleteToken(ctx, source.ID, deleted); err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, oldToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted token authenticated")
	}
	if _, _, err := s.Accept(ctx, oldToken, raw, m, deleted); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted token persisted telemetry")
	}
	if _, _, err := s.ShowToken(ctx, source.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleted token was shown")
	}
	sources, err := s.Sources(ctx)
	if err != nil || len(sources) != 1 {
		t.Fatal("deletion lost the source")
	}
	got := sources[0]
	if got.ID != source.ID || got.Name != source.Name || got.CreatedAt != source.CreatedAt || got.HasToken || got.LastSeen != Timestamp(created) || got.TokenCreatedAt != Timestamp(created) || got.TokenDeletedAt != Timestamp(deleted) {
		t.Fatalf("source identity or token metadata changed: %+v", got)
	}
	var encrypted, hash []byte
	if err := s.db.QueryRow("SELECT token_encrypted, token_hash FROM telemetry_sources WHERE id = ?", source.ID).Scan(&encrypted, &hash); err != nil {
		t.Fatal(err)
	}
	if encrypted != nil || hash != nil {
		t.Fatal("deleted token credentials retained")
	}
	replaced := deleted.Add(time.Minute)
	if err := s.CreateToken(ctx, source.ID, replaced); err != nil {
		t.Fatal(err)
	}
	_, newToken, err := s.ShowToken(ctx, source.ID)
	if err != nil || newToken == oldToken {
		t.Fatal("replacement was not a new credential")
	}
	if err := s.Authenticate(ctx, newToken); err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, oldToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("replacement resurrected old token")
	}
	// Duplicate identity and history are scoped to the source, across tokens.
	retry, duplicate, err := s.Accept(ctx, newToken, raw, m, replaced)
	if err != nil || !duplicate || retry.ID != first.ID {
		t.Fatalf("replacement lost deduplication history: %v", err)
	}
	newRaw := []byte(`{"schema_version":1,"boot_id":"b","telemetry_sequence":2}`)
	newMetadata, _ := telemetry.Parse(newRaw)
	if _, _, err := s.Accept(ctx, newToken, newRaw, newMetadata, replaced); err != nil {
		t.Fatal(err)
	}
	uploads, err := s.Uploads(ctx, Filter{SourceID: source.ID})
	if err != nil || len(uploads) != 2 {
		t.Fatal("history did not remain attached to the source")
	}
	for _, upload := range uploads {
		if upload.SourceID != source.ID || upload.SourceName != source.Name {
			t.Fatal("history changed ownership")
		}
	}
	sources, err = s.Sources(ctx)
	if err != nil || len(sources) != 1 || !sources[0].HasToken || sources[0].TokenCreatedAt != Timestamp(replaced) || sources[0].TokenDeletedAt != "" {
		t.Fatal("replacement did not reset token metadata")
	}
	if err := s.CreateToken(ctx, source.ID+1, replaced); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("created token without a source")
	}
}

func TestConcurrentTokenCreation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	source, err := s.CreateSource(ctx, "A", testTime)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- s.CreateToken(ctx, source.ID, testTime) }()
	}
	close(start)
	created, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			created++
		} else if errors.Is(err, ErrTokenExists) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatal("concurrent creation replaced a credential")
	}
	_, token, err := s.ShowToken(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}
}

func TestExportStopsOnCancellation(t *testing.T) {
	s := testStore(t)
	_, token := createTestSource(t, s, "A")
	raw := []byte(`{"schema_version":1}`)
	m, _ := telemetry.Parse(raw)
	for range 3 {
		if _, _, err := s.Accept(context.Background(), token, raw, m, testTime); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := 0
	err := s.Export(ctx, Filter{}, func(Upload) error { writes++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || writes != 1 {
		t.Fatalf("export continued after cancellation: writes=%d, err=%v", writes, err)
	}
}
