package telemetry

import "testing"

func TestFlexibleSchemaAndSummary(t *testing.T) {
	raw := []byte(`{"schema_version":1,"boot_id":"b","telemetry_sequence":9007199254740993,"unknown":{"future":[1,2]},"system":{"uptime_seconds":86400,"firmware_version":"test","build":{"git":"abc"}},"heap":{"internal":{"free_bytes":123},"psram":{"free_bytes":456,"minimum_free_bytes":12,"largest_free_block":234}},"modbus":{"new_provider":{"registers":{"999999":{"value":42}}}}}`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.TelemetrySequence != "9007199254740993" || m.Uptime != "86400" || m.Firmware != "test" || m.Build != "abc" || m.Summary.InternalFree != "123" || m.Summary.PSRAMFree != "456" || m.Summary.PSRAMMinimum != "12" || m.DedupeKey == "" {
		t.Fatalf("incorrect metadata: %+v", m)
	}
	if _, err := Parse([]byte(`{"schema_version":1,"diagnostics":["future format"],"storage":null}`)); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidJSONAndSchema(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", "{", `{}`, `{"schema_version":2}`, `{"schema_version":"1"}`, `{"schema_version":{}}`, `{"schema_version":1} {}`, "{\"schema_version\":1,\"bad\":\"\xff\"}"} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid payload %q", raw)
		}
	}
}

func TestDedupeIdentity(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		dedupe bool
	}{
		{`{"schema_version":1}`, false},
		{`{"schema_version":1,"telemetry_sequence":1}`, false},
		{`{"schema_version":1,"boot_id":"b"}`, false},
		{`{"schema_version":1,"upload_id":"unique"}`, true},
		{`{"schema_version":1,"boot_id":"b","telemetry_sequence":0}`, true},
		{`{"schema_version":1,"boot_id":"b","checkpoint_sequence":4}`, true},
	} {
		m, err := Parse([]byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if (m.DedupeKey != "") != tc.dedupe {
			t.Fatalf("wrong dedupe behavior for %s", tc.raw)
		}
	}
}
