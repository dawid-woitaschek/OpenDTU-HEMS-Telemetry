package database

import (
	"context"
	"database/sql"
	"errors"
)

const schemaVersion = 3

// The v1 schema is retained as the initial migration so new and existing
// databases take the same transactional upgrade path.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS telemetry_sources (
 id INTEGER PRIMARY KEY,
 name TEXT NOT NULL,
 token_encrypted BLOB,
 token_hash BLOB UNIQUE,
 created_at TEXT NOT NULL,
 last_seen_at TEXT,
 revoked_at TEXT,
 CHECK ((token_encrypted IS NULL) = (token_hash IS NULL))
);
CREATE TABLE IF NOT EXISTS telemetry_uploads (
 id INTEGER PRIMARY KEY,
 source_id INTEGER NOT NULL REFERENCES telemetry_sources(id),
 received_at TEXT NOT NULL,
 schema_version INTEGER NOT NULL,
 boot_id TEXT NOT NULL,
 upload_id TEXT NOT NULL,
 uptime TEXT NOT NULL,
 firmware TEXT NOT NULL,
 build_identity TEXT NOT NULL,
 reset_reason TEXT NOT NULL,
 telemetry_sequence TEXT NOT NULL,
 checkpoint_sequence TEXT NOT NULL,
 dedupe_key TEXT,
 summary_json TEXT NOT NULL,
 raw_json BLOB NOT NULL,
 UNIQUE(source_id, dedupe_key)
);
CREATE INDEX IF NOT EXISTS uploads_source_id ON telemetry_uploads(source_id, id DESC);
CREATE INDEX IF NOT EXISTS uploads_received ON telemetry_uploads(received_at);
PRAGMA user_version = 1;`

func migrate(tx *sql.Tx) error {
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > schemaVersion {
		return errors.New("unsupported database schema version")
	}
	if version == 0 {
		if _, err := tx.Exec(schemaV1); err != nil {
			return err
		}
		version = 1
	}
	if version == 1 {
		// Renaming and adding columns leaves source IDs, ciphertext (whose AAD is
		// the source ID), upload rows, foreign keys and indexes untouched.
		_, err := tx.Exec(`
ALTER TABLE telemetry_sources RENAME COLUMN revoked_at TO token_deleted_at;
ALTER TABLE telemetry_sources ADD COLUMN token_created_at TEXT;
UPDATE telemetry_sources SET token_encrypted = NULL, token_hash = NULL WHERE token_deleted_at IS NOT NULL;
UPDATE telemetry_sources SET token_created_at = created_at WHERE token_hash IS NOT NULL;
PRAGMA user_version = 2;`)
		if err != nil {
			return err
		}
	}
	if version <= 2 {
		if _, err := tx.Exec(dashboardSchema); err != nil {
			return err
		}
		if err := backfillDashboard(context.Background(), tx); err != nil {
			return err
		}
		if _, err := tx.Exec("PRAGMA user_version = 3"); err != nil {
			return err
		}
	}
	return nil
}
