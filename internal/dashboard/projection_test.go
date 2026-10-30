package dashboard

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func rich(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/rich-hems.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestRichFirmwareProjection(t *testing.T) {
	raw := rich(t)
	s := Parse(raw, time.Unix(1793372400, 0))
	p := s.Point
	if p.TimeBasis != "hems_capture" || p.CapturedAt != "2026-10-30T14:00:00Z" {
		t.Fatalf("capture: %+v", p)
	}
	if *p.Grid.Value != -420 || *p.Storage.Value != 800 || *p.EVSE.Value != 0 {
		t.Fatal("sign semantics or genuine zero changed")
	}
	if *p.Market != 21.25 || *p.Import != 31.25 {
		t.Fatal("micros/kWh must divide by 10000 for ct/kWh")
	}
	if *p.SOC.Value != 62 || p.SOC.Quality != "fresh" || p.TariffQuality != "fresh" {
		t.Fatal("normalized signals")
	}
	if *p.Shadow != -800 || p.ShadowAction != "set_discharge_power_w" || *p.Planned != -800 {
		t.Fatal("firmware preview/plan")
	}
	if len(s.Schedule) != 6 || *s.Schedule[0].Price != 31.25 || *p.InternalFree != 161200 {
		t.Fatal("schedule/runtime")
	}
}
func TestNullZeroAndQuality(t *testing.T) {
	for _, tc := range []struct {
		quality string
		value   any
		known   bool
	}{{"fresh", 0., true}, {"stale", -25., true}, {"unknown", 0., false}, {"fresh", nil, false}, {"future_quality", 12., false}} {
		raw, _ := json.Marshal(map[string]any{"hems": map[string]any{"normalized": map[string]any{"grid": map[string]any{"power_w": map[string]any{"quality": tc.quality, "value": tc.value}}}}})
		p := Parse(raw, time.Now()).Point
		if (p.Grid.Value != nil) != tc.known {
			t.Fatalf("%+v: %+v", tc, p.Grid)
		}
		if tc.known && p.Grid.Quality != tc.quality {
			t.Fatal("quality lost")
		}
		if p.SOC.Value != nil || p.EVSE.Value != nil || p.Import != nil || p.Shadow != nil {
			t.Fatal("unavailable became zero")
		}
	}
}
func TestLegacyFallbackAndUnavailableShadow(t *testing.T) {
	received := time.Date(2026, 10, 30, 16, 0, 0, 123, time.UTC)
	for _, capture := range []string{"null", "0", "\"invalid\""} {
		p := Parse([]byte(`{"hems":{"captured_at_utc":`+capture+`,"shadow_if_active":{"valid":false,"power_w":0,"action":"Stop","reason":"device_unreachable"}}}`), received).Point
		if p.TimeBasis != "collector_receive_fallback" || p.CapturedAt != received.Format(time.RFC3339Nano) || p.Shadow != nil || p.ShadowAction != "" {
			t.Fatal(p)
		}
	}
}
func TestFreshness(t *testing.T) {
	now := time.Now()
	cadence := 900.
	for _, tc := range []struct {
		age     time.Duration
		cadence *float64
		want    string
	}{{time.Minute, &cadence, "fresh"}, {time.Hour, &cadence, "stale"}, {3 * 24 * time.Hour, &cadence, "stale"}, {time.Minute, nil, "unknown"}, {-time.Hour, &cadence, "unknown"}} {
		if got := Freshness(now.Add(-tc.age).Format(time.RFC3339Nano), now, tc.cadence); got != tc.want {
			t.Fatalf("%+v got %s", tc, got)
		}
	}
}
func TestProjectionDoesNotForwardUnknownObjectsOrSecrets(t *testing.T) {
	var root map[string]any
	json.Unmarshal(rich(t), &root)
	root["Authorization"] = "SECRET"
	root["token_hash"] = "SECRET"
	h := root["hems"].(map[string]any)
	h["api_token"] = "SECRET"
	h["control"].(map[string]any)["password"] = "SECRET"
	root["system"].(map[string]any)["admin_credentials"] = "SECRET"
	raw, _ := json.Marshal(root)
	encoded, _ := json.Marshal(Parse(raw, time.Now()))
	if strings.Contains(string(encoded), "SECRET") {
		t.Fatal("secret leaked into allowlisted projection")
	}
}

func TestTariffQualityWithoutFlagsIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		flags string
		want  string
	}{{``, "unknown"}, {`,"fresh":false,"current_covered":true`, "stale"}, {`,"fresh":true,"current_covered":true`, "fresh"}} {
		p := Parse([]byte(`{"hems":{"tariff":{"current":{"price_micros_per_kwh":0,"import_price_micros_per_kwh":0}`+tc.flags+`}}}`), time.Now()).Point
		if p.TariffQuality != tc.want || p.Market == nil || *p.Market != 0 || p.Import == nil || *p.Import != 0 {
			t.Fatal(tc, p)
		}
	}
}
