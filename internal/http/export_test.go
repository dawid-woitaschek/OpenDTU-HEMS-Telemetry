package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"opendtu-hems-telemetry/internal/telemetry"
)

// Simulate a connection whose old request deadline has expired between chunks.
// No sleeps are needed to prove that each streaming write renews its deadline.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline  time.Time
	deadlines []time.Time
	writes    int
	onWrite   func()
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineRecorder) Write(b []byte) (int, error) {
	if !w.deadline.IsZero() && time.Now().After(w.deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	w.writes++
	n, err := w.ResponseRecorder.Write(b)
	w.deadline = time.Now().Add(-time.Second)
	if w.onWrite != nil {
		w.onWrite()
	}
	return n, err
}

func seedLargeExport(t *testing.T, f *fixture) {
	t.Helper()
	noise := make([]byte, 128*1024)
	if _, err := rand.New(rand.NewSource(42)).Read(noise); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"schema_version":1,"raw_diagnostics":"%s"}`, hex.EncodeToString(noise)))
	m, err := telemetry.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, err := f.store.Accept(context.Background(), f.token, raw, m, fixedTime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestZIPExportRenewsWriteDeadlinePerChunk(t *testing.T) {
	f := setup(t)
	seedLargeExport(t, f)
	r := httptest.NewRequest("GET", "/downloads/export.zip", nil)
	r.SetBasicAuth("admin", "test-password")
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), deadline: time.Now().Add(-time.Second)}
	started := time.Now()
	f.handler.ServeHTTP(w, r)
	requireStatus(t, w.ResponseRecorder, 200)
	if w.writes < 2 || len(w.deadlines) != w.writes+1 {
		t.Fatal("export did not renew the deadline at start and for each chunk")
	}
	for _, deadline := range w.deadlines {
		if deadline.IsZero() || deadline.Before(started.Add(exportWriteIdleTimeout)) || deadline.After(time.Now().Add(exportWriteIdleTimeout)) {
			t.Fatal("export disabled deadlines or used an unbounded write window")
		}
	}
	z, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil || len(z.File) != 6 {
		t.Fatalf("export was cut off by the previous request deadline: %v", err)
	}
}

func TestWriteDeadlineOverrideIsLimitedToValidAuthenticatedExport(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		path   string
		admin  bool
		status int
	}{
		{"/uploads", true, 200},
		{"/health", false, 200},
		{"/downloads/export.zip", false, 401},
		{"/downloads/export.zip?from=invalid", true, 400},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.admin {
			r.SetBasicAuth("admin", "test-password")
		}
		w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), deadline: time.Now().Add(time.Minute)}
		f.handler.ServeHTTP(w, r)
		requireStatus(t, w.ResponseRecorder, tc.status)
		if len(w.deadlines) != 0 {
			t.Fatal("ordinary or rejected request changed its write deadline")
		}
	}
}

func TestZIPExportStopsWritingWhenClientContextIsCanceled(t *testing.T) {
	f := setup(t)
	seedLargeExport(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("GET", "/downloads/export.zip", nil).WithContext(ctx)
	r.SetBasicAuth("admin", "test-password")
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), onWrite: cancel}
	f.handler.ServeHTTP(w, r)
	if w.writes != 1 {
		t.Fatalf("export continued writing after disconnect: %d writes", w.writes)
	}
	if _, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err == nil {
		t.Fatal("canceled export appeared complete")
	}
}
