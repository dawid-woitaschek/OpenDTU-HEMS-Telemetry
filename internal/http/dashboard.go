package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"opendtu-hems-telemetry/internal/database"
)

func dashboardFilter(r *http.Request, now time.Time, live bool) (database.Filter, error) {
	var f database.Filter
	id, err := strconv.ParseInt(r.URL.Query().Get("sender"), 10, 64)
	if err != nil || id < 1 {
		return f, errors.New("select a valid sender")
	}
	f.SourceID = id
	if live {
		return f, nil
	}
	q := r.URL.Query()
	duration := 24 * time.Hour
	switch q.Get("range") {
	case "", "24h":
	case "6h":
		duration = 6 * time.Hour
	case "7d":
		duration = 7 * 24 * time.Hour
	case "30d":
		duration = 30 * 24 * time.Hour
	case "custom":
		from, e1 := time.Parse(time.RFC3339, q.Get("from"))
		to, e2 := time.Parse(time.RFC3339, q.Get("to"))
		if e1 != nil || e2 != nil || !from.Before(to) {
			return f, errors.New("custom range requires RFC3339 from < to (exclusive)")
		}
		f.From = database.Timestamp(from)
		f.Until = database.Timestamp(to)
		return f, nil
	default:
		return f, errors.New("invalid range")
	}
	f.From = database.Timestamp(now.Add(-duration))
	f.Until = database.Timestamp(now.Add(time.Nanosecond))
	return f, nil
}
func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.Sources(r.Context())
	if err != nil {
		s.failure(w, "list dashboard senders")
		return
	}
	view := r.URL.Path
	title := map[string]string{"/overview": "Overview", "/live": "Live View", "/hems": "HEMS", "/runtime": "Runtime", "/devices": "Devices"}[view]
	s.render(w, "dashboard", page{Title: title, View: view, Sources: sources})
}
func (s *Server) dashboardData(w http.ResponseWriter, r *http.Request) {
	live := r.URL.Path == "/dashboard/live"
	now := time.Now()
	f, err := dashboardFilter(r, now, live)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := s.store.Dashboard(r.Context(), f, live, now)
	if err != nil {
		s.failure(w, "read dashboard")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(data)
}
