// Package dashboard builds a versioned, allowlisted view of authoritative raw JSON.
package dashboard

import (
	"encoding/json"
	"strings"
	"time"
)

const Version = 1

type Signal struct {
	Value   *float64 `json:"value"`
	Quality string   `json:"quality"`
}
type Point struct {
	PlanValid       *bool    `json:"plan_valid"`
	ID              int64    `json:"id"`
	CapturedAt      string   `json:"captured_at"`
	TimeBasis       string   `json:"time_basis"`
	ReceivedAt      string   `json:"received_at"`
	Bytes           int      `json:"bytes"`
	Grid            Signal   `json:"grid"`
	PV              Signal   `json:"pv"`
	Storage         Signal   `json:"storage"`
	SOC             Signal   `json:"soc"`
	EVSE            Signal   `json:"evse"`
	Market          *float64 `json:"market_ct"`
	Import          *float64 `json:"import_ct"`
	TariffQuality   string   `json:"tariff_quality"`
	Planned         *float64 `json:"planned_w"`
	Shadow          *float64 `json:"shadow_w"`
	ShadowAction    string   `json:"shadow_action"`
	Trigger         []string `json:"trigger"`
	InternalFree    *float64 `json:"internal_free"`
	InternalLargest *float64 `json:"internal_largest"`
	InternalMinimum *float64 `json:"internal_minimum"`
	PSRAMFree       *float64 `json:"psram_free"`
}
type Field struct {
	Path  string `json:"path"`
	Value any    `json:"value"`
}
type ScheduleRow struct {
	Start   *float64 `json:"start"`
	End     *float64 `json:"end"`
	Battery *float64 `json:"battery_w"`
	SOC     *float64 `json:"end_soc_pct"`
	PV      *float64 `json:"pv_w"`
	Load    *float64 `json:"assumed_load_w"`
	Price   *float64 `json:"import_ct"`
	Action  string   `json:"action"`
}
type Snapshot struct {
	Point    Point         `json:"point"`
	Fields   []Field       `json:"fields"`
	Schedule []ScheduleRow `json:"schedule"`
}

func value(root map[string]any, path string) any {
	var v any = root
	for _, key := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}
func number(v any) *float64 {
	n, ok := v.(float64)
	if !ok {
		return nil
	}
	return &n
}
func text(v any) string { s, _ := v.(string); return s }
func ct(v any) *float64 {
	n := number(v)
	if n != nil {
		*n /= 10000
	}
	return n
}
func signal(root map[string]any, path string) Signal {
	q := text(value(root, path+".quality"))
	n := number(value(root, path+".value"))
	if (q != "fresh" && q != "stale") || n == nil {
		return Signal{nil, "unknown"}
	}
	return Signal{n, q}
}
func stringsOnly(v any) []string {
	out := []string{}
	if rows, ok := v.([]any); ok {
		for _, row := range rows {
			if s, ok := row.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// Parse intentionally ignores unknown fields. No entire firmware sub-object is
// forwarded: adding future firmware credentials cannot expose them in this API.
func Parse(raw []byte, received time.Time) Snapshot {
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	p := Point{CapturedAt: received.UTC().Format(time.RFC3339Nano), ReceivedAt: received.UTC().Format(time.RFC3339Nano), TimeBasis: "collector_receive_fallback", Bytes: len(raw)}
	// Current firmware sends Unix seconds, not an RFC3339 string. Accept RFC3339
	// too for imported records; zero/unsynchronised device clocks use receive time.
	capture := value(root, "hems.captured_at_utc")
	var t time.Time
	if n := number(capture); n != nil && *n >= 946684800 && *n < 253402300800 {
		t = time.Unix(int64(*n), 0)
	} else if s := text(capture); s != "" {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	if !t.IsZero() {
		p.CapturedAt = t.UTC().Format(time.RFC3339Nano)
		p.TimeBasis = "hems_capture"
	}
	p.Grid = signal(root, "hems.normalized.grid.power_w")
	p.PV = signal(root, "hems.normalized.pv.ac_power_w")
	p.Storage = signal(root, "hems.normalized.storage.ac_power_w")
	p.SOC = signal(root, "hems.normalized.storage.soc_pct")
	p.EVSE = signal(root, "hems.normalized.evse.power_w")
	p.Market = ct(value(root, "hems.tariff.current.price_micros_per_kwh"))
	p.Import = ct(value(root, "hems.tariff.current.import_price_micros_per_kwh"))
	p.TariffQuality = "unknown"
	if p.Market != nil || p.Import != nil {
		fresh := value(root, "hems.tariff.fresh")
		covered := value(root, "hems.tariff.current_covered")
		if fresh == false || covered == false {
			p.TariffQuality = "stale"
		}
		if fresh == true && covered == true {
			p.TariffQuality = "fresh"
		}
	}
	p.Planned = number(value(root, "hems.optimizer.current_planned_battery_w"))
	if valid, ok := value(root, "hems.optimizer.plan_valid").(bool); ok {
		p.PlanValid = &valid
	}
	if value(root, "hems.shadow_if_active.valid") == true {
		p.Shadow = number(value(root, "hems.shadow_if_active.power_w"))
		p.ShadowAction = text(value(root, "hems.shadow_if_active.action"))
	}
	p.Trigger = stringsOnly(value(root, "hems.trigger"))
	p.InternalFree = number(value(root, "heap.internal.free_bytes"))
	p.InternalLargest = number(value(root, "heap.internal.largest_free_block"))
	p.InternalMinimum = number(value(root, "heap.internal.minimum_free_bytes"))
	p.PSRAMFree = number(value(root, "heap.psram.free_bytes"))
	s := Snapshot{Point: p, Fields: []Field{}, Schedule: []ScheduleRow{}}
	for _, path := range fieldPaths {
		v := value(root, path)
		switch v.(type) {
		case nil:
			continue
		case string, float64, bool:
			s.Fields = append(s.Fields, Field{path, v})
		case []any:
			s.Fields = append(s.Fields, Field{path, stringsOnly(v)})
		}
	}
	// Task names and watermarks are projected as scalars; no arbitrary system data.
	if tasks, ok := value(root, "system.task_details").([]any); ok {
		for _, v := range tasks {
			if task, ok := v.(map[string]any); ok {
				if name := text(task["name"]); name != "" {
					s.Fields = append(s.Fields, Field{"system.stack_watermark_bytes." + name, number(task["stack_watermark"])})
				}
			}
		}
	}
	if checkpoints, ok := value(root, "diagnostics.checkpoints").([]any); ok && len(checkpoints) > 0 {
		if checkpoint, ok := checkpoints[len(checkpoints)-1].(map[string]any); ok {
			s.Fields = append(s.Fields, Field{"diagnostics.last_checkpoint_stack_free_bytes", number(checkpoint["stack_free_bytes"])})
		}
	}
	if rows, ok := value(root, "hems.optimizer.schedule").([]any); ok {
		for _, v := range rows {
			row, ok := v.(map[string]any)
			if !ok {
				continue
			}
			s.Schedule = append(s.Schedule, ScheduleRow{number(row["start"]), number(row["end"]), number(row["battery_w"]), number(row["end_soc_pct"]), number(row["pv_w"]), number(row["assumed_load_w"]), ct(row["import_price_micros_per_kwh"]), text(row["action"])})
		}
	}
	return s
}

// Fixed scalar paths only. Unknown nested data stays exclusively in raw JSON.
var fieldPaths = strings.Fields(`
hems.global_mode hems.control.storage.mode hems.control.evse.mode
hems.control.storage.automatic.writes_possible hems.control.storage.automatic.blockers
hems.control.evse.automatic.writes_possible hems.control.evse.automatic.blockers
hems.optimizer.plan_valid hems.optimizer.plan_revision hems.optimizer.state
hems.optimizer.plan_reason hems.optimizer.plan_blocker hems.optimizer.replan_reason hems.optimizer.price_basis
hems.optimizer.current_planned_battery_w hems.optimizer.stale_inputs
hems.optimizer.optimized_cost_nano hems.optimizer.baseline_cost_nano hems.optimizer.modeled_savings_nano
hems.optimizer.currency hems.optimizer.cost_breakdown.degradation_nano
hems.optimizer.cost_breakdown.terminal_inventory_change_nano hems.optimizer.retained_by_hysteresis
hems.optimizer.load_forecast_source hems.optimizer.config.expected_base_load_w
hems.optimizer.config.min_soc_pct hems.optimizer.config.max_soc_pct hems.optimizer.config.max_charge_w
hems.optimizer.config.max_discharge_w hems.optimizer.config.policies.grid_charging_allowed
hems.optimizer.config.energy_management.pv_export hems.optimizer.config.energy_management.battery_export
hems.optimizer.warnings hems.optimizer.assumptions hems.optimizer.blockers hems.optimizer.policy_restrictions
hems.optimizer.tariff_usable hems.optimizer.forecast_source hems.optimizer.schedule_truncated
hems.economic_dispatch.coordinated_battery_w hems.economic_dispatch.state hems.economic_dispatch.coordination_state
hems.shadow_if_active.valid hems.shadow_if_active.action hems.shadow_if_active.power_w hems.shadow_if_active.reason
hems.constraints.applicable.charge_allowed hems.constraints.applicable.discharge_allowed
hems.constraints.applicable.max_charge_power_w hems.constraints.applicable.max_discharge_power_w
hems.constraints.physical.known hems.constraints.physical.max_charge_w hems.constraints.physical.max_discharge_w
hems.constraints.ev_protection.state hems.flow.dpl_available hems.flow.dpl_fresh
storage.enabled storage.reachable storage.stale storage.has_update storage.age_ms storage.failed_polls storage.error
wallbox.enabled wallbox.reachable wallbox.stale wallbox.has_update wallbox.age_ms wallbox.failed_polls wallbox.error
hems.tariff.has_data hems.tariff.enabled hems.tariff.provider hems.tariff.fresh hems.tariff.planning_usable hems.tariff.state
hems.tariff.last_error hems.tariff.transport_age_seconds hems.tariff.fetched_at_utc
hems.forecast.has_data hems.forecast.enabled hems.forecast.provider hems.forecast.fresh hems.forecast.planning_usable hems.forecast.state
hems.forecast.last_error hems.forecast.transport_age_seconds hems.forecast.fetched_at_utc
system.uptime system.uptime_seconds system.reset_reason system.resetreason_0 system.git_hash system.network.connected
telemetry_sequence checkpoint_sequence diagnostics.allocation_failure_count diagnostics.dropped_samples
 diagnostics.dropped_allocation_failures diagnostics.expired_samples diagnostics.expired_allocation_failures
 diagnostics.serialized_checkpoint_count diagnostics.checkpoint_batch_truncated diagnostics.runtime.heap.failed_alloc_count
`)

// Freshness describes wall-clock age independently of the firmware's per-signal
// quality. Cadence is estimated from captures; no firmware cadence is invented.
func Freshness(captured string, now time.Time, cadenceSeconds *float64) string {
	t, err := time.Parse(time.RFC3339Nano, captured)
	if err != nil {
		return "unknown"
	}
	age := now.Sub(t).Seconds()
	if age < -60 {
		return "unknown"
	}
	if cadenceSeconds == nil || *cadenceSeconds <= 0 {
		return "unknown"
	}
	if age > *cadenceSeconds*2+60 {
		return "stale"
	}
	return "fresh"
}
