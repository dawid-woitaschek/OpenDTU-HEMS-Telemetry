package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"opendtu-hems-telemetry/internal/auth"
	"opendtu-hems-telemetry/internal/database"
	"opendtu-hems-telemetry/internal/telemetry"
	"opendtu-hems-telemetry/web"
)

const MaxTelemetryBytes int64 = 1024 * 1024 // 1 MiB, including whitespace

const exportWriteIdleTimeout = 60 * time.Second

type Server struct {
	store              *database.Store
	username, password string
	csrf               string
	pages              *template.Template
	logger             *log.Logger
}

func New(store *database.Store, username, password string, logger *log.Logger) (http.Handler, error) {
	if username == "" || password == "" {
		return nil, errors.New("ADMIN_USERNAME and ADMIN_PASSWORD are required")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	pages, err := template.New("pages").Funcs(template.FuncMap{"uptime": uptime, "shorttime": shortTime}).ParseFS(web.Files, "*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{store: store, username: username, password: password, csrf: base64.RawURLEncoding.EncodeToString(b), pages: pages, logger: logger}
	admin := http.NewServeMux()
	admin.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/uploads", http.StatusSeeOther) })
	admin.HandleFunc("GET /sources", s.sources)
	admin.HandleFunc("POST /sources", s.createSource)
	admin.HandleFunc("POST /sources/{id}/token", s.showToken)
	admin.HandleFunc("POST /sources/{id}/token/create", s.createToken)
	admin.HandleFunc("POST /sources/{id}/token/delete", s.deleteToken)
	admin.HandleFunc("GET /uploads", s.uploads)
	admin.HandleFunc("GET /uploads/{id}", s.detail)
	admin.HandleFunc("GET /uploads/{id}/download", s.download)
	admin.HandleFunc("GET /downloads", s.downloadPage)
	admin.HandleFunc("GET /downloads/export.zip", s.export)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/api/v1/telemetry", s.ingest)
	mux.HandleFunc("/api/", http.NotFound)
	mux.Handle("/", s.adminAuth(admin))
	return securityHeaders(mux), nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		userOK, passwordOK := auth.Equal(username, s.username), auth.Equal(password, s.password)
		if !ok || !userOK || !passwordOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="OpenDTU-HEMS telemetry", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	token, ok := auth.Bearer(r.Header.Get("Authorization"))
	if !ok {
		deviceUnauthorized(w)
		return
	}
	if err := s.store.Authenticate(r.Context(), token); err != nil {
		if errors.Is(err, database.ErrUnauthorized) {
			deviceUnauthorized(w)
		} else {
			s.failure(w, "authenticate source")
		}
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		http.Error(w, "Content-Encoding is not supported", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxTelemetryBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload exceeds 1 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "could not read payload", http.StatusBadRequest)
		}
		return
	}
	m, err := telemetry.Parse(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, duplicate, err := s.store.Accept(r.Context(), token, raw, m, time.Now())
	if err != nil {
		if errors.Is(err, database.ErrUnauthorized) {
			deviceUnauthorized(w)
		} else {
			s.failure(w, "store telemetry")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	checkpoint := u.Metadata.AcceptedCheckpointSequence
	if m.AcceptedCheckpointSequence == nil {
		checkpoint = nil
	}
	json.NewEncoder(w).Encode(struct {
		Accepted                   bool    `json:"accepted"`
		ReceivedAt                 string  `json:"received_at"`
		Duplicate                  bool    `json:"duplicate,omitempty"`
		AcceptedCheckpointSequence *uint64 `json:"accepted_checkpoint_sequence,omitempty"`
	}{true, u.ReceivedAt, duplicate, checkpoint})
}

func deviceUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "invalid or missing device token", http.StatusUnauthorized)
}

type page struct {
	Title          string
	CSRF           string
	Sources        []database.Source
	Uploads        []database.Upload
	Source         database.Source
	Upload         database.Upload
	Token          string
	JSON           string
	SourceID       int64
	From, To, Next string
}

func (s *Server) render(w http.ResponseWriter, name string, data page) {
	data.CSRF = s.csrf
	var buf bytes.Buffer
	if err := s.pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.failure(w, "render admin page")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) failure(w http.ResponseWriter, operation string) {
	// Deliberately exclude request headers, URLs, payloads, credentials and SQL
	// errors from logs and responses.
	if s.logger != nil {
		s.logger.Printf("operation failed: %s", operation)
	}
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (s *Server) form(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		http.Error(w, "unsupported form content type", http.StatusUnsupportedMediaType)
		return false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	if !auth.Equal(r.PostForm.Get("csrf"), s.csrf) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "invalid CSRF token; reload the page", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.Sources(r.Context())
	if err != nil {
		s.failure(w, "list sources")
		return
	}
	s.render(w, "sources", page{Title: "Sources / Tokens", Sources: sources})
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	if !s.form(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 200 || strings.ContainsAny(name, "\x00\r\n") {
		http.Error(w, "name must contain 1–200 characters on one line", http.StatusBadRequest)
		return
	}
	if _, err := s.store.CreateSource(r.Context(), name, time.Now()); err != nil {
		s.failure(w, "create source")
		return
	}
	http.Redirect(w, r, "/sources", http.StatusSeeOther)
}

func requestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func (s *Server) showToken(w http.ResponseWriter, r *http.Request) {
	if !s.form(w, r) {
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	source, token, err := s.store.ShowToken(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.failure(w, "show source token")
		return
	}
	s.render(w, "token", page{Title: "API token", Source: source, Token: token})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	if !s.form(w, r) {
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	if err := s.store.CreateToken(r.Context(), id, time.Now()); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			http.NotFound(w, r)
		case errors.Is(err, database.ErrTokenExists):
			http.Error(w, "source already has an active token", http.StatusConflict)
		default:
			s.failure(w, "create source token")
		}
		return
	}
	http.Redirect(w, r, "/sources", http.StatusSeeOther)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	if !s.form(w, r) {
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteToken(r.Context(), id, time.Now()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			s.failure(w, "delete source token")
		}
		return
	}
	http.Redirect(w, r, "/sources", http.StatusSeeOther)
}

// UI dates are explicit UTC calendar days. The upper bound includes the whole
// selected day by querying a half-open interval ending at the following day.
func parseFilter(r *http.Request, cursor bool) (database.Filter, error) {
	var f database.Filter
	q := r.URL.Query()
	if v := q.Get("source_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			return f, errors.New("invalid source_id")
		}
		f.SourceID = id
	}
	var from, to time.Time
	var err error
	if v := q.Get("from"); v != "" {
		from, err = time.Parse("2006-01-02", v)
		if err != nil {
			return f, errors.New("from must be YYYY-MM-DD (UTC)")
		}
		f.From = database.Timestamp(from)
	}
	if v := q.Get("to"); v != "" {
		to, err = time.Parse("2006-01-02", v)
		if err != nil || to.Year() >= 9999 {
			return f, errors.New("to must be YYYY-MM-DD (UTC), before year 9999")
		}
		f.Until = database.Timestamp(to.AddDate(0, 0, 1))
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return f, errors.New("from must not be after to")
	}
	if cursor {
		if v := q.Get("before"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id < 1 {
				return f, errors.New("invalid cursor")
			}
			f.BeforeID = id
		}
	}
	return f, nil
}

func (s *Server) uploads(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uploads, err := s.store.Uploads(r.Context(), f)
	if err != nil {
		s.failure(w, "list uploads")
		return
	}
	sources, err := s.store.Sources(r.Context())
	if err != nil {
		s.failure(w, "list sources")
		return
	}
	data := page{Title: "Telemetry uploads", Sources: sources, Uploads: uploads, SourceID: f.SourceID, From: r.URL.Query().Get("from"), To: r.URL.Query().Get("to")}
	if len(uploads) > 100 {
		data.Uploads = uploads[:100]
		q := r.URL.Query()
		q.Set("before", strconv.FormatInt(uploads[99].ID, 10))
		data.Next = "/uploads?" + q.Encode()
	}
	s.render(w, "uploads", data)
}

func (s *Server) loadUpload(w http.ResponseWriter, r *http.Request) (database.Upload, bool) {
	id, ok := requestID(w, r)
	if !ok {
		return database.Upload{}, false
	}
	u, err := s.store.Upload(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return u, false
	}
	if err != nil {
		s.failure(w, "read upload")
		return u, false
	}
	return u, true
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, u.Raw, "", "  "); err != nil {
		s.failure(w, "format upload")
		return
	}
	s.render(w, "detail", page{Title: fmt.Sprintf("Upload #%d", u.ID), Upload: u, JSON: pretty.String()})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="telemetry-%d.json"`, u.ID))
	w.Header().Set("Content-Length", strconv.Itoa(len(u.Raw)))
	w.Write(u.Raw)
}

func (s *Server) downloadPage(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.Sources(r.Context())
	if err != nil {
		s.failure(w, "list export sources")
		return
	}
	s.render(w, "downloads", page{Title: "Download telemetry", Sources: sources})
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Replace the ordinary per-request write deadline with a sliding deadline
	// only for this authenticated export. Each write gets a bounded window;
	// a progressing download can outlive the server's global WriteTimeout.
	output := &exportWriter{ResponseWriter: w, controller: http.NewResponseController(w), ctx: r.Context()}
	if err := output.renewDeadline(); err != nil {
		s.failure(w, "set export write deadline")
		return
	}
	// ZIP entries are the exact received bytes; names contain only server IDs.
	// Sidecars supply source and receive-time metadata without modifying device
	// payloads or trusting source names as filesystem paths.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="telemetry.zip"`)
	z := zip.NewWriter(output)
	err = s.store.Export(r.Context(), f, func(u database.Upload) error {
		h := &zip.FileHeader{Name: fmt.Sprintf("source-%d/telemetry-%d.json", u.SourceID, u.ID), Method: zip.Deflate}
		h.SetModTime(parseTime(u.ReceivedAt))
		entry, err := z.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err = entry.Write(u.Raw); err != nil {
			return err
		}
		entry, err = z.Create(fmt.Sprintf("metadata/upload-%d.json", u.ID))
		if err != nil {
			return err
		}
		return json.NewEncoder(entry).Encode(struct {
			UploadID   int64  `json:"upload_id"`
			SourceID   int64  `json:"source_id"`
			SourceName string `json:"source_name"`
			ReceivedAt string `json:"received_at"`
		}{u.ID, u.SourceID, u.SourceName, u.ReceivedAt})
	})
	// Finish the archive only on success: a failed download stays detectably
	// truncated rather than looking like a complete export with missing records.
	if err != nil {
		if !output.started {
			w.Header().Del("Content-Disposition")
			w.Header().Del("Content-Type")
			s.failure(w, "stream export")
		} else {
			s.exportFailure()
		}
		return
	}
	if err = z.Close(); err != nil {
		s.exportFailure()
	}
}

type exportWriter struct {
	http.ResponseWriter
	started    bool
	controller *http.ResponseController
	ctx        context.Context
}

func (w *exportWriter) renewDeadline() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	err := w.controller.SetWriteDeadline(time.Now().Add(exportWriteIdleTimeout))
	// ResponseRecorder and other non-network writers have no socket deadline.
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w *exportWriter) Write(b []byte) (int, error) {
	if err := w.renewDeadline(); err != nil {
		return 0, err
	}
	w.started = true
	return w.ResponseWriter.Write(b)
}

func (w *exportWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) exportFailure() {
	if s.logger != nil {
		s.logger.Print("operation failed: stream export")
	}
}

func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }
func shortTime(v string) string {
	if v == "" {
		return "never"
	}
	return parseTime(v).UTC().Format("2006-01-02 15:04:05")
}
func uptime(v string) string {
	seconds, err := strconv.ParseFloat(v, 64)
	if err != nil || seconds < 0 || seconds >= math.MaxInt64 || math.IsNaN(seconds) {
		return v
	}
	s := int64(seconds)
	return fmt.Sprintf("%dh %02dm %02ds", s/3600, (s/60)%60, s%60)
}
