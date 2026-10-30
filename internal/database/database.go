package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"opendtu-hems-telemetry/internal/auth"
	"opendtu-hems-telemetry/internal/telemetry"

	_ "modernc.org/sqlite"
)

var ErrUnauthorized = errors.New("invalid or deleted source token")
var ErrTokenExists = errors.New("source already has an active token")

// Fixed-width UTC timestamps also sort correctly as SQLite TEXT.
const timestampFormat = "2006-01-02T15:04:05.000000000Z"

func Timestamp(t time.Time) string { return t.UTC().Format(timestampFormat) }

type Store struct {
	db     *sql.DB
	cipher *auth.Cipher
}

type Source struct {
	ID             int64
	Name           string
	CreatedAt      string
	LastSeen       string
	HasToken       bool
	TokenCreatedAt string
	TokenDeletedAt string
}

type Upload struct {
	ID         int64
	SourceID   int64
	SourceName string
	ReceivedAt string
	Metadata   telemetry.Metadata
	Raw        []byte
}

type Filter struct {
	SourceID int64
	From     string // inclusive, fixed-width UTC
	Until    string // exclusive, fixed-width UTC
	BeforeID int64  // cursor for the upload list
}

func Open(path string, tokenCipher *auth.Cipher) (*Store, error) {
	if tokenCipher == nil {
		return nil, errors.New("token cipher is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{}
	for _, pragma := range []string{"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)", "temp_store(MEMORY)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &Store{db: db, cipher: tokenCipher}
	if err = s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrate(tx); err != nil {
		return err
	}
	// Verify the key before committing a migration or accepting new credentials.
	if err := s.verifyTokens(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) verifyTokens(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT id, token_encrypted, token_hash FROM telemetry_sources WHERE token_hash IS NOT NULL")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var encrypted, digest []byte
		if err := rows.Scan(&id, &encrypted, &digest); err != nil {
			return err
		}
		token, err := s.cipher.Decrypt(encrypted, id)
		if err != nil {
			return err
		}
		if !auth.Equal(string(auth.Digest(token)), string(digest)) {
			return errors.New("source token integrity check failed")
		}
	}
	return rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) CreateSource(ctx context.Context, name string, now time.Time) (Source, error) {
	created := Timestamp(now)
	result, err := s.db.ExecContext(ctx, "INSERT INTO telemetry_sources(name, created_at) VALUES (?, ?)", name, created)
	if err != nil {
		return Source{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Source{}, err
	}
	return Source{ID: id, Name: name, CreatedAt: created}, nil
}

// CreateToken fills an empty credential slot; it never replaces an active token.
// The conditional update also protects against concurrent creation requests.
func (s *Store) CreateToken(ctx context.Context, sourceID int64, now time.Time) error {
	token, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	encrypted, err := s.cipher.Encrypt(token, sourceID)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE telemetry_sources SET token_encrypted = ?, token_hash = ?, token_created_at = ?, token_deleted_at = NULL WHERE id = ? AND token_hash IS NULL`, encrypted, auth.Digest(token), Timestamp(now), sourceID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 0 {
		return err
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM telemetry_sources WHERE id = ?", sourceID).Scan(&id); err != nil {
		return err
	}
	return ErrTokenExists
}

func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, COALESCE(last_seen_at, ''), token_hash IS NOT NULL, COALESCE(token_created_at, ''), COALESCE(token_deleted_at, '') FROM telemetry_sources ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ID, &src.Name, &src.CreatedAt, &src.LastSeen, &src.HasToken, &src.TokenCreatedAt, &src.TokenDeletedAt); err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

func (s *Store) ShowToken(ctx context.Context, id int64) (Source, string, error) {
	var source Source
	var encrypted []byte
	err := s.db.QueryRowContext(ctx, "SELECT id, name, token_encrypted FROM telemetry_sources WHERE id = ? AND token_hash IS NOT NULL", id).Scan(&source.ID, &source.Name, &encrypted)
	if err != nil {
		return source, "", err
	}
	token, err := s.cipher.Decrypt(encrypted, id)
	return source, token, err
}

func (s *Store) DeleteToken(ctx context.Context, id int64, now time.Time) error {
	result, err := s.db.ExecContext(ctx, "UPDATE telemetry_sources SET token_encrypted = NULL, token_hash = NULL, token_deleted_at = ? WHERE id = ? AND token_hash IS NOT NULL", Timestamp(now), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) Authenticate(ctx context.Context, token string) error {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM telemetry_sources WHERE token_hash = ?", auth.Digest(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	return err
}

// Accept rechecks credentials in the write transaction. A request that was
// authenticated before its body was read cannot bypass a completed revocation.
func (s *Store) Accept(ctx context.Context, token string, raw []byte, m telemetry.Metadata, now time.Time) (Upload, bool, error) {
	var upload Upload
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return upload, false, err
	}
	defer tx.Rollback()
	received := Timestamp(now)
	var sourceID int64
	err = tx.QueryRowContext(ctx, `UPDATE telemetry_sources SET last_seen_at = CASE WHEN last_seen_at > ? THEN last_seen_at ELSE ? END WHERE token_hash = ? RETURNING id`, received, received, auth.Digest(token)).Scan(&sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return upload, false, ErrUnauthorized
	}
	if err != nil {
		return upload, false, err
	}
	var dedupe any
	if m.DedupeKey != "" {
		dedupe = m.DedupeKey
	}
	summary, err := json.Marshal(m.Summary)
	if err != nil {
		return upload, false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO telemetry_uploads (
source_id, received_at, schema_version, boot_id, upload_id, uptime, firmware, build_identity, reset_reason, telemetry_sequence, checkpoint_sequence, dedupe_key, summary_json, raw_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(source_id, dedupe_key) DO NOTHING`,
		sourceID, received, m.SchemaVersion, m.BootID, m.UploadID, m.Uptime, m.Firmware, m.Build, m.ResetReason, m.TelemetrySequence, m.CheckpointSequence, dedupe, string(summary), raw)
	if err != nil {
		return upload, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return upload, false, err
	}
	duplicate := n == 0
	var id int64
	if duplicate {
		err = tx.QueryRowContext(ctx, "SELECT id, received_at, raw_json FROM telemetry_uploads WHERE source_id = ? AND dedupe_key = ?", sourceID, dedupe).Scan(&id, &received, &raw)
		if err == nil {
			m, err = telemetry.Parse(raw)
		}
	} else {
		id, err = result.LastInsertId()
		if err == nil {
			err = projectUpload(ctx, tx, id, sourceID, raw, received)
		}
	}
	if err != nil {
		return upload, false, err
	}
	if err = tx.Commit(); err != nil {
		return upload, false, err
	}
	return Upload{ID: id, SourceID: sourceID, ReceivedAt: received, Metadata: m}, duplicate, nil
}

const uploadColumns = `u.id, u.source_id, s.name, u.received_at, u.schema_version, u.boot_id, u.upload_id, u.uptime, u.firmware, u.build_identity, u.reset_reason, u.telemetry_sequence, u.checkpoint_sequence, u.summary_json`

func scanUpload(row interface{ Scan(...any) error }, withRaw bool) (Upload, error) {
	var u Upload
	var summary string
	args := []any{&u.ID, &u.SourceID, &u.SourceName, &u.ReceivedAt, &u.Metadata.SchemaVersion, &u.Metadata.BootID, &u.Metadata.UploadID, &u.Metadata.Uptime, &u.Metadata.Firmware, &u.Metadata.Build, &u.Metadata.ResetReason, &u.Metadata.TelemetrySequence, &u.Metadata.CheckpointSequence, &summary}
	if withRaw {
		args = append(args, &u.Raw)
	}
	if err := row.Scan(args...); err != nil {
		return u, err
	}
	err := json.Unmarshal([]byte(summary), &u.Metadata.Summary)
	return u, err
}

func (s *Store) Upload(ctx context.Context, id int64) (Upload, error) {
	return scanUpload(s.db.QueryRowContext(ctx, "SELECT "+uploadColumns+", u.raw_json FROM telemetry_uploads u JOIN telemetry_sources s ON s.id = u.source_id WHERE u.id = ?", id), true)
}

func where(f Filter) (string, []any) {
	conditions := []string{"1=1"}
	var args []any
	if f.SourceID != 0 {
		conditions = append(conditions, "u.source_id = ?")
		args = append(args, f.SourceID)
	}
	if f.From != "" {
		conditions = append(conditions, "u.received_at >= ?")
		args = append(args, f.From)
	}
	if f.Until != "" {
		conditions = append(conditions, "u.received_at < ?")
		args = append(args, f.Until)
	}
	if f.BeforeID != 0 {
		conditions = append(conditions, "u.id < ?")
		args = append(args, f.BeforeID)
	}
	return strings.Join(conditions, " AND "), args
}

func (s *Store) Uploads(ctx context.Context, f Filter) ([]Upload, error) {
	condition, args := where(f)
	rows, err := s.db.QueryContext(ctx, "SELECT "+uploadColumns+" FROM telemetry_uploads u JOIN telemetry_sources s ON s.id = u.source_id WHERE "+condition+" ORDER BY u.id DESC LIMIT 101", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var uploads []Upload
	for rows.Next() {
		u, err := scanUpload(rows, false)
		if err != nil {
			return nil, err
		}
		uploads = append(uploads, u)
	}
	return uploads, rows.Err()
}

// Export streams rows to keep memory bounded regardless of collection size.
func (s *Store) Export(ctx context.Context, f Filter, write func(Upload) error) error {
	condition, args := where(f)
	rows, err := s.db.QueryContext(ctx, "SELECT u.id, u.source_id, s.name, u.received_at, u.raw_json FROM telemetry_uploads u JOIN telemetry_sources s ON s.id = u.source_id WHERE "+condition+" ORDER BY u.id", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u Upload
		if err := rows.Scan(&u.ID, &u.SourceID, &u.SourceName, &u.ReceivedAt, &u.Raw); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := write(u); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("export interrupted: %w", err)
	}
	return nil
}
