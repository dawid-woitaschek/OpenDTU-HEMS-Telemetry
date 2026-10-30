package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"opendtu-hems-telemetry/internal/dashboard"
)

const dashboardSchema = `CREATE TABLE dashboard_snapshots (
 upload_id INTEGER PRIMARY KEY REFERENCES telemetry_uploads(id) ON DELETE CASCADE,
 source_id INTEGER NOT NULL REFERENCES telemetry_sources(id),
 captured_at TEXT NOT NULL, received_at TEXT NOT NULL,
 projection_version INTEGER NOT NULL, point_json TEXT NOT NULL, detail_json TEXT NOT NULL
);
CREATE INDEX dashboard_capture ON dashboard_snapshots(source_id, captured_at, upload_id);
CREATE INDEX dashboard_received ON dashboard_snapshots(source_id, received_at DESC, upload_id DESC);`

func projectUpload(ctx context.Context, tx *sql.Tx, id, sourceID int64, raw []byte, received string) error {
	t, _ := time.Parse(time.RFC3339Nano, received)
	s := dashboard.Parse(raw, t)
	s.Point.ID = id
	p, err := json.Marshal(s.Point)
	if err != nil {
		return err
	}
	detail, err := json.Marshal(s)
	if err != nil {
		return err
	}
	capture, _ := time.Parse(time.RFC3339Nano, s.Point.CapturedAt)
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO dashboard_snapshots VALUES (?,?,?,?,?,?,?)`, id, sourceID, Timestamp(capture), received, dashboard.Version, string(p), string(detail))
	return err
}
func backfillDashboard(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,source_id,received_at,raw_json FROM telemetry_uploads ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, source int64
		var received string
		var raw []byte
		if err = rows.Scan(&id, &source, &received, &raw); err != nil {
			return err
		}
		if err = projectUpload(ctx, tx, id, source, raw, received); err != nil {
			return err
		}
	}
	return rows.Err()
}

// RebuildDashboard atomically recreates all derived values from original bytes.
// It never changes uploads, sender identities, token state or dedupe identities.
func (s *Store) RebuildDashboard(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM dashboard_snapshots"); err != nil {
		return err
	}
	if err = backfillDashboard(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

type DashboardData struct {
	Points         []dashboard.Point   `json:"points"`
	Current        *dashboard.Snapshot `json:"current"`
	CadenceSeconds *float64            `json:"cadence_seconds"`
	Freshness      string              `json:"freshness"`
	Truncated      bool                `json:"truncated"`
	Limit          int                 `json:"limit"`
	Now            string              `json:"now"`
}

// Dashboard uses capture-time filters, unlike raw upload/export receive filters.
// Live selects by receive order, so a delayed transmission is visibly a delayed
// capture rather than silently replacing its capture timestamp with arrival time.
func (s *Store) Dashboard(ctx context.Context, f Filter, live bool, now time.Time) (DashboardData, error) {
	out := DashboardData{Points: []dashboard.Point{}, Limit: 10000, Now: now.UTC().Format(time.RFC3339Nano), Freshness: "unknown"}
	conditions := []string{"source_id = ?"}
	args := []any{f.SourceID}
	if !live {
		if f.From != "" {
			conditions = append(conditions, "captured_at >= ?")
			args = append(args, f.From)
		}
		if f.Until != "" {
			conditions = append(conditions, "captured_at < ?")
			args = append(args, f.Until)
		}
	}
	order := "captured_at DESC, upload_id DESC"
	limit := out.Limit + 1
	if live {
		order = "received_at DESC, upload_id DESC"
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx, "SELECT point_json FROM dashboard_snapshots WHERE "+strings.Join(conditions, " AND ")+" ORDER BY "+order+" LIMIT ?", append(args, limit)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return out, err
		}
		var p dashboard.Point
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			rows.Close()
			return out, err
		}
		out.Points = append(out.Points, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Points) > out.Limit {
		out.Truncated = true
		out.Points = out.Points[:out.Limit]
	}
	if len(out.Points) == 0 {
		return out, nil
	}
	var detail string
	if err = s.db.QueryRowContext(ctx, "SELECT detail_json FROM dashboard_snapshots WHERE upload_id = ?", out.Points[0].ID).Scan(&detail); err != nil {
		return out, err
	}
	out.Current = new(dashboard.Snapshot)
	if err = json.Unmarshal([]byte(detail), out.Current); err != nil {
		return out, err
	}
	// Cadence is an estimate from up to 32 distinct recent captures for this
	// sender, independent of receive retries and the selected historical range.
	cadenceRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT captured_at FROM dashboard_snapshots WHERE source_id=? ORDER BY captured_at DESC LIMIT 32`, f.SourceID)
	if err != nil {
		return out, err
	}
	var previous time.Time
	var intervals []float64
	for cadenceRows.Next() {
		var stamp string
		if err = cadenceRows.Scan(&stamp); err != nil {
			break
		}
		t, _ := time.Parse(time.RFC3339Nano, stamp)
		if !previous.IsZero() {
			d := previous.Sub(t).Seconds()
			if d > 0 {
				intervals = append(intervals, d)
			}
		}
		previous = t
	}
	rowErr := cadenceRows.Err()
	cadenceRows.Close()
	if err != nil {
		return out, err
	}
	if rowErr != nil {
		return out, rowErr
	}
	if len(intervals) > 0 {
		sort.Float64s(intervals)
		v := intervals[len(intervals)/2]
		out.CadenceSeconds = &v
	}
	out.Freshness = dashboard.Freshness(out.Current.Point.CapturedAt, now, out.CadenceSeconds)
	// SQL order is canonical fixed-width UTC; reverse without re-sorting the
	// RFC3339 display strings (which can have varying fractional precision).
	for i, j := 0, len(out.Points)-1; i < j; i, j = i+1, j-1 {
		out.Points[i], out.Points[j] = out.Points[j], out.Points[i]
	}
	return out, nil
}
