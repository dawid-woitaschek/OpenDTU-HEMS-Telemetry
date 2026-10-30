package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"opendtu-hems-telemetry/internal/database"
	"opendtu-hems-telemetry/internal/telemetry"
)

func (f *fixture) capture(raw []byte, received time.Time, token string) int64 {
	f.t.Helper()
	m, err := telemetry.Parse(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	u, _, err := f.store.Accept(context.Background(), token, raw, m, received)
	if err != nil {
		f.t.Fatal(err)
	}
	return u.ID
}
func (f *fixture) dashboard(path string) database.DashboardData {
	f.t.Helper()
	w := f.request("GET", path, "", "admin", "")
	requireStatus(f.t, w, 200)
	var d database.DashboardData
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		f.t.Fatal(err)
	}
	return d
}
func firmwareFixture(t *testing.T) []byte {
	t.Helper()
	raw, e := os.ReadFile("../dashboard/testdata/rich-hems.json")
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func TestDashboardCaptureOrderCurrentRangeLiveAndIsolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	rich := firmwareFixture(t)
	// Newest received record is an older capture: delayed retry semantics.
	newestCapture := f.capture(rich, fixedTime, f.token)
	older := []byte(strings.ReplaceAll(strings.ReplaceAll(string(rich), "1793368800", "1793365200"), `"telemetry_sequence": 42`, `"telemetry_sequence": 43`))
	delayed := f.capture(older, fixedTime.Add(time.Hour), f.token)
	second, err := f.store.CreateSource(ctx, "Other house", fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.CreateToken(ctx, second.ID, fixedTime); err != nil {
		t.Fatal(err)
	}
	_, otherToken, _ := f.store.ShowToken(ctx, second.ID)
	f.capture(rich, fixedTime.Add(2*time.Hour), otherToken)
	base := fmt.Sprintf("/dashboard/data?sender=%d&range=custom&from=2026-10-30T12:00:00Z&to=2026-10-30T15:00:00Z", f.source.ID)
	d := f.dashboard(base)
	if len(d.Points) != 2 || d.Points[0].ID != delayed || d.Points[1].ID != newestCapture || d.Current.Point.ID != newestCapture {
		t.Fatalf("capture order/current/isolation: %+v", d)
	}
	narrow := f.dashboard(fmt.Sprintf("/dashboard/data?sender=%d&range=custom&from=2026-10-30T12:00:00Z&to=2026-10-30T14:00:00Z", f.source.ID))
	if len(narrow.Points) != 1 || narrow.Current.Point.ID != delayed {
		t.Fatal("current must be newest inside half-open capture range")
	}
	live := f.dashboard(fmt.Sprintf("/dashboard/live?sender=%d&range=custom&from=bad&to=bad", f.source.ID))
	if live.Current.Point.ID != delayed || len(live.Points) != 1 {
		t.Fatal("live must ignore historical filters and use receive order")
	}
	if live.Current.Point.CapturedAt == live.Current.Point.ReceivedAt {
		t.Fatal("delayed receive replaced capture time")
	}
	isolated := f.dashboard(fmt.Sprintf("/dashboard/live?sender=%d", second.ID))
	if isolated.Current.Point.ID == delayed {
		t.Fatal("cross-sender leak")
	}
	if err = f.store.RebuildDashboard(ctx); err != nil {
		t.Fatal(err)
	}
	rebuilt := f.dashboard(base)
	before, _ := json.Marshal(d.Current)
	after, _ := json.Marshal(rebuilt.Current)
	if string(before) != string(after) {
		t.Fatal("projection is not reproducible")
	}
	u, err := f.store.Upload(ctx, newestCapture)
	if err != nil || string(u.Raw) != string(rich) {
		t.Fatal("rebuild changed raw JSON")
	}
}
func TestDashboardLegacyFallbackAndNoSecrets(t *testing.T) {
	f := setup(t)
	raw := []byte(`{"schema_version":1,"Authorization":"secret-sentinel","api_token":"secret-sentinel","system":{"password":"secret-sentinel"},"hems":{"optimizer":{"api_token":"secret-sentinel"}}}`)
	f.capture(raw, fixedTime, f.token)
	path := fmt.Sprintf("/dashboard/live?sender=%d", f.source.ID)
	d := f.dashboard(path)
	if d.Current.Point.TimeBasis != "collector_receive_fallback" || d.Current.Point.SOC.Value != nil || d.Current.Point.EVSE.Value != nil {
		t.Fatal("legacy/unavailable handling")
	}
	w := f.request("GET", path, "", "admin", "")
	for _, secret := range []string{"secret-sentinel", f.token, "token_hash", "token_encrypted", "raw_json"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("dashboard exposed %s", secret)
		}
	}
	if !strings.Contains(w.Body.String(), `"value":null`) {
		t.Fatal("unknown not serialized as null")
	}
}
func TestDashboardAuthenticationAndPages(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/overview", "/live", "/hems", "/runtime", "/devices", "/dashboard/data?sender=1", "/dashboard/live?sender=1", "/assets/dashboard.js"} {
		for _, credentials := range []string{"", f.token} {
			w := f.request("GET", path, "", credentials, "")
			requireStatus(t, w, http.StatusUnauthorized)
		}
		w := f.request("GET", path, "", "admin", "")
		requireStatus(t, w, http.StatusOK)
	}
	w := f.request("GET", "/sources", "", "admin", "")
	if strings.Contains(w.Body.String(), `placeholder="Schwägerin"`) || !strings.Contains(w.Body.String(), "Create sender") {
		t.Fatal("sender terminology")
	}
}
func TestDashboardFilterValidation(t *testing.T) {
	now := fixedTime
	for _, query := range []string{"sender=bad", "sender=-1", "sender=1&range=bad", "sender=1&range=custom", "sender=1&range=custom&from=2026-10-31T00:00:00Z&to=2026-10-30T00:00:00Z"} {
		r, _ := http.NewRequest("GET", "http://collector/dashboard/data?"+query, nil)
		if _, err := dashboardFilter(r, now, false); err == nil {
			t.Fatal(query)
		}
	}
	for _, tc := range []struct {
		rangeName string
		hours     int
	}{{"6h", 6}, {"24h", 24}, {"7d", 168}, {"30d", 720}} {
		r, _ := http.NewRequest("GET", "http://collector/dashboard/data?sender=2&range="+tc.rangeName, nil)
		filter, err := dashboardFilter(r, now, false)
		if err != nil || filter.SourceID != 2 || filter.From != database.Timestamp(now.Add(-time.Duration(tc.hours)*time.Hour)) {
			t.Fatal(tc, filter, err)
		}
	}
}

func TestDashboardDedupeRetainsOriginalProjection(t *testing.T) {
	f := setup(t)
	raw := firmwareFixture(t)
	id := f.capture(raw, fixedTime, f.token)
	changed := []byte(strings.ReplaceAll(string(raw), `"value": -420`, `"value": 0`))
	m, err := telemetry.Parse(changed)
	if err != nil {
		t.Fatal(err)
	}
	u, duplicate, err := f.store.Accept(context.Background(), f.token, changed, m, fixedTime.Add(time.Hour))
	if err != nil || !duplicate || u.ID != id {
		t.Fatal("dedupe identity changed", err)
	}
	d := f.dashboard(fmt.Sprintf("/dashboard/data?sender=%d&range=custom&from=2026-10-30T12:00:00Z&to=2026-10-31T00:00:00Z", f.source.ID))
	if len(d.Points) != 1 || *d.Current.Point.Grid.Value != -420 {
		t.Fatal("retry changed authoritative projected values")
	}
}

func TestDashboardFractionalCaptureOrdering(t *testing.T) {
	f := setup(t)
	// Lexicographic RFC3339 strings with variable precision do NOT sort correctly;
	// the indexed projection uses fixed-width UTC timestamps instead.
	first := f.capture([]byte(`{"schema_version":1,"hems":{"captured_at_utc":"2026-10-30T14:00:00.100Z"}}`), fixedTime, f.token)
	second := f.capture([]byte(`{"schema_version":1,"hems":{"captured_at_utc":"2026-10-30T14:00:00Z"}}`), fixedTime.Add(time.Minute), f.token)
	d := f.dashboard(fmt.Sprintf("/dashboard/data?sender=%d&range=custom&from=2026-10-30T14:00:00Z&to=2026-10-30T14:00:01Z", f.source.ID))
	if len(d.Points) != 2 || d.Points[0].ID != second || d.Current.Point.ID != first {
		t.Fatal("fractional capture ordering changed")
	}
}
