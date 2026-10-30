// Package telemetry extracts optional hints without imposing a nested schema.
// The original request bytes remain authoritative.
package telemetry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Metadata struct {
	SchemaVersion              int
	BootID                     string
	UploadID                   string
	Uptime                     string
	Firmware                   string
	Build                      string
	ResetReason                string
	TelemetrySequence          string
	CheckpointSequence         string
	AcceptedCheckpointSequence *uint64
	DedupeKey                  string
	Summary                    Summary
}

type Summary struct {
	InternalFree     string `json:"internal_free,omitempty"`
	InternalMinimum  string `json:"internal_minimum,omitempty"`
	InternalLargest  string `json:"internal_largest,omitempty"`
	PSRAMFree        string `json:"psram_free,omitempty"`
	PSRAMMinimum     string `json:"psram_minimum,omitempty"`
	PSRAMLargest     string `json:"psram_largest,omitempty"`
	AllocationFails  string `json:"allocation_failures,omitempty"`
	StorageSOC       string `json:"storage_soc,omitempty"`
	WallboxConnected string `json:"wallbox_connected,omitempty"`
	WallboxCharging  string `json:"wallbox_charging,omitempty"`
}

func Parse(raw []byte) (Metadata, error) {
	var m Metadata
	if !utf8.Valid(raw) {
		return m, errors.New("payload must be UTF-8 JSON")
	}
	var root map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&root); err != nil || root == nil {
		return m, errors.New("payload must be a JSON object")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, errors.New("payload must contain exactly one JSON object")
	}
	if root["schema_version"] != json.Number("1") {
		return m, errors.New("schema_version must be the integer 1")
	}
	m.SchemaVersion = 1
	m.BootID = first(root, "boot_id", "system.boot_id")
	m.UploadID = first(root, "upload_id")
	m.Uptime = first(root, "system.uptime_seconds", "uptime_seconds", "system.uptime", "uptime")
	m.Firmware = first(root, "system.firmware_version", "system.firmware.version", "firmware.version", "firmware_version")
	m.Build = first(root, "system.build.git", "system.git_hash", "system.build_id", "build_id", "system.build", "build")
	m.ResetReason = first(root, "system.reset_reason.name", "system.reset_reason", "reset_reason")
	m.TelemetrySequence = first(root, "telemetry_sequence", "sequence")
	m.CheckpointSequence = first(root, "checkpoint_sequence", "diagnostics.checkpoint_sequence")
	// Acknowledgements use only non-negative JSON integer counters. Other
	// optional representations remain in the raw payload and metadata hints.
	if value, ok := firstValue(root, "checkpoint_sequence", "diagnostics.checkpoint_sequence").(json.Number); ok {
		if sequence, err := strconv.ParseUint(string(value), 10, 64); err == nil {
			m.AcceptedCheckpointSequence = &sequence
		}
	}
	m.Summary = Summary{
		InternalFree:     first(root, "heap.internal.free_bytes", "heap.internal.free", "memory.internal.free_bytes"),
		InternalMinimum:  first(root, "heap.internal.minimum_free_bytes", "heap.internal.min_free_bytes", "memory.internal.minimum_free_bytes"),
		InternalLargest:  first(root, "heap.internal.largest_free_block", "memory.internal.largest_free_block"),
		PSRAMFree:        first(root, "heap.psram.free_bytes", "psram.free_bytes", "memory.psram.free_bytes"),
		PSRAMMinimum:     first(root, "heap.psram.minimum_free_bytes", "heap.psram.min_free_bytes", "psram.minimum_free_bytes", "memory.psram.minimum_free_bytes"),
		PSRAMLargest:     first(root, "heap.psram.largest_free_block", "psram.largest_free_block", "memory.psram.largest_free_block"),
		AllocationFails:  first(root, "diagnostics.counters.allocation_failures", "diagnostics.allocation_failure_count"),
		StorageSOC:       first(root, "storage.state_of_charge", "storage.soc", "storage.normalized.state_of_charge"),
		WallboxConnected: first(root, "wallbox.connection_state", "wallbox.connected", "wallbox.normalized.connection_state"),
		WallboxCharging:  first(root, "wallbox.charging_state", "wallbox.charging", "wallbox.normalized.charging_state"),
	}
	// Upload IDs identify a transmission, scoped to the boot when provided.
	// Bare counters are unsafe across reboots, so they need a boot ID.
	var identity []string
	switch {
	case m.UploadID != "":
		identity = []string{"upload", m.BootID, m.UploadID}
	case m.BootID != "" && m.TelemetrySequence != "":
		identity = []string{"telemetry", m.BootID, m.TelemetrySequence}
	case m.BootID != "" && m.CheckpointSequence != "":
		identity = []string{"checkpoint", m.BootID, m.CheckpointSequence}
	}
	if identity != nil {
		b, _ := json.Marshal(identity)
		h := sha256.Sum256(b)
		m.DedupeKey = hex.EncodeToString(h[:])
	}
	return m, nil
}

func first(root map[string]any, paths ...string) string {
	switch value := firstValue(root, paths...).(type) {
	case string:
		return value
	case json.Number:
		return string(value)
	case bool:
		return strconv.FormatBool(value)
	}
	return ""
}

func firstValue(root map[string]any, paths ...string) any {
	for _, path := range paths {
		var v any = root
		for _, part := range strings.Split(path, ".") {
			obj, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = obj[part]
		}
		switch value := v.(type) {
		case string:
			if value != "" {
				return value
			}
		case json.Number:
			return value
		case bool:
			return value
		}
	}
	return nil
}
