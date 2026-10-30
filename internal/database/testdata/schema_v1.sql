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
PRAGMA user_version = 1;
