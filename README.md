# OpenDTU-HEMS telemetry collector

A small receiver for unattended OpenDTU-HEMS field tests, typically one snapshot
per hour. Devices send diagnostic JSON over HTTPS with a source-specific Bearer
token. The collector records the original bytes in SQLite and provides a simple,
password-protected UI for inspection and download. It does not control devices,
interpret storage/EVSE registers, chart data, or automatically delete snapshots.

## Architecture

```text
OpenDTU-HEMS ── HTTPS / Bearer ──> Dockyard's existing Traefik
                                        │ Docker network: proxy
                                        ▼
                                 hems-telemetry:8080
                                        │
                                 /data/telemetry.db
```

One Go process, the standard HTTP/template libraries, and a pure Go SQLite
driver (`modernc.org/sqlite`, the only direct external dependency). HTML templates
are embedded in the binary. The runtime image is a static binary in `scratch`,
running as UID/GID **65532:65532**. It supports Linux ARM64 without CGO.

SQLite has two tables: `telemetry_sources` and `telemetry_uploads`. Uploads retain
source ID, server receive time, schema version, optional boot/upload/sequence,
uptime, firmware/build and reset metadata, a small diagnostic summary, and the
authoritative raw JSON BLOB. WAL mode and full synchronous writes are enabled.
The application performs no automatic telemetry deletion.

## Configuration

Copy `.env.example` to `.env` and fill in every value. There are no default admin
credentials or encryption keys.

| Variable | Required | Meaning |
| --- | --- | --- |
| `TELEMETRY_HOST` | Compose | Public hostname only, e.g. your chosen DNS name, without a scheme/path. |
| `ADMIN_USERNAME` | Yes | Single administrator's Basic Auth username; cannot contain `:`. |
| `ADMIN_PASSWORD` | Yes | Strong administrator password. |
| `TOKEN_ENCRYPTION_KEY` | Yes | Standard base64 encoding of exactly 32 cryptographically random bytes. |
| `DATA_DIR` | No | Database directory; defaults to `/data` and is fixed there in Compose. |

Generate the master key with:

```sh
openssl rand -base64 32
```

Generate an independent administrator password, for example with
`openssl rand -hex 24`. Put the generated values in `.env`, protect it with
`chmod 600 .env`, and keep a secure separate backup of the master key. `.env`,
database files, and the data directory are git-ignored and Docker-ignored.

Tokens contain 256 random bits, encoded with a `hems_` prefix. They are stored
using AES-256-GCM with a fresh nonce and source ID as authenticated associated
data. A SHA-256 token digest provides indexed authentication; it is not the only
stored representation. Token plaintext is only returned by an authenticated,
CSRF-protected **Show** action. The application does not log credentials,
authorization headers, or payloads.

The same master key must be retained across restarts and restores. Startup
verifies existing active tokens and fails if they cannot be decrypted. Changing
the environment key is not a key rotation procedure; this initial version has
no rotation tool.

## Deploy on Dockyard

Dockyard needs a 64-bit Linux/ARM64 OS, Docker Engine, Docker Compose, the existing
Traefik instance, and its existing external `proxy` network. Point the public DNS
hostname at Dockyard and use Traefik's existing HTTPS/certificate setup.

From this repository on Dockyard:

```sh
cp .env.example .env
chmod 600 .env
# Edit .env with your hostname, admin credentials, and generated master key.
cp compose.example.yml compose.yml
mkdir -p data
sudo chown 65532:65532 data
sudo chmod 700 data
docker network inspect proxy
docker compose up -d --build
docker compose ps
```

The example uses `build: .`, `restart: unless-stopped`, `./data:/data`, and
`platform: linux/arm64`. It attaches to `proxy` and **does not publish a host
port**. Traefik's labels select the `proxy` network and container port 8080.
The root filesystem is read-only; SQLite writes only to `/data`.

The labels assume an HTTPS entrypoint named `websecure`. Adapt it to Dockyard's
existing name if needed. If Traefik requires an explicit certificate resolver,
uncomment the example `tls.certresolver` label and use the existing resolver's
name. No additional reverse proxy is required. Restrict the service to Traefik's
HTTPS router: Basic Auth credentials and device tokens rely on HTTPS in transit.

Verify the public endpoint:

```sh
curl --fail --silent --show-error "https://${TELEMETRY_HOST}/health"
```

It returns `ok` without authentication. The container healthcheck runs
`/collector healthcheck`, which checks the local HTTP endpoint and SQLite
connectivity without needing a shell or curl in the runtime image.

## Create the first source and send a snapshot

1. Open `https://<your-host>/sources` and supply the configured administrator
   credentials when the browser prompts.
2. Enter a name such as **Schwägerin** and select **Create source**. The source
   is a permanent logical identity and initially has no API token.
3. Select **Create token** for that source, then **Show token** to copy the
   generated credential and configure it on the sending device. Each source
   has at most one active token; creation cannot replace an active credential.
4. **Delete token** immediately invalidates that credential. The source ID,
   name, last-seen time, and historical uploads remain. **Create token** becomes
   available again and issues a new credential for the **same source**. Its
   history and duplicate detection continue across token replacements; the
   previous token stays invalid. There is no source deletion action.

For these shell examples, set `TELEMETRY_HOST` to your public hostname and
`ADMIN_USERNAME` to your configured username; Compose's `.env` is not
automatically loaded into your shell. Read the device token without adding it
to shell history (Bash):

```sh
read -r -s -p 'Device API token: ' DEVICE_TOKEN
printf '\n'
curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer ${DEVICE_TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/telemetry-v1.json \
  "https://${TELEMETRY_HOST}/api/v1/telemetry"
unset DEVICE_TOKEN
```

The acknowledgement is compact:

```json
{"accepted":true,"received_at":"2026-10-30T14:05:00.000000000Z","accepted_checkpoint_sequence":128}
```

`accepted_checkpoint_sequence` acknowledges the request's `checkpoint_sequence`
(or `diagnostics.checkpoint_sequence`) only after successful database commit.
Use an unsigned 64-bit decimal JSON integer, including zero. Missing counters,
strings, negative/fractional/exponent values, booleans and out-of-range values
are not acknowledged; the otherwise valid snapshot and all its raw fields are
still stored. The checkpoint field is omitted when no valid counter is present.
A rejected request or failed database transaction never receives
a successful checkpoint acknowledgement.

The example uses fixed boot/upload/sequence IDs. Repeatedly sending it to the
same source is intentionally deduplicated; change the identifiers to simulate
new snapshots.

## Schema version 1

`POST /api/v1/telemetry` accepts `Content-Type: application/json` (optional media
type parameters are allowed), requires Bearer authentication, and accepts at most
**1 MiB** of UTF-8 JSON including whitespace. Content encodings such as gzip are
not supported. The body must be exactly one JSON object with:

```json
{"schema_version":1}
```

That is the only required telemetry field. Nested objects are intentionally
flexible. Unknown fields, complete diagnostics, INTERNAL heap history, PSRAM
history, normalized and raw storage/EVSE state, and arbitrary Modbus providers
and register/value pairs are preserved. Optional metadata is extracted on a
best-effort basis; unrecognized shapes remain available in the raw payload.
No supplied source/owner/name field can override the authenticated source.

A compact illustrative payload:

```json
{
  "schema_version": 1,
  "boot_id": "boot-unique-id",
  "telemetry_sequence": 42,
  "checkpoint_sequence": 128,
  "system": {
    "uptime_seconds": 83640,
    "firmware_version": "field-test",
    "build": {"git": "example", "platformio_environment": "esp32s3-example"},
    "chip": {"model": "ESP32-S3"},
    "reset_reason": "POWERON_RESET",
    "network": {"connected": true}
  },
  "diagnostics": {
    "context": "hourly_snapshot",
    "boot": {"stage": "running"},
    "previous_boot": {"last_checkpoint": "loop_idle"},
    "counters": {"allocation_failures": 0},
    "checkpoints": [{"sequence": 128}],
    "stack_watermarks": {"loop_task_bytes": 4096}
  },
  "heap": {
    "internal": {"free_bytes": 132000, "minimum_free_bytes": 118000, "largest_free_block": 96000},
    "psram": {"total_bytes": 8388608, "free_bytes": 7340032, "minimum_free_bytes": 7000000, "largest_free_block": 7200000}
  },
  "storage": {
    "provider": "marstek", "connection_state": "connected", "state_of_charge": 58,
    "charge_power_w": 350, "raw": {"provider_status": 1}
  },
  "wallbox": {
    "provider": "warp", "connection_state": "connected", "charging_state": "charging",
    "requested_current_a": 6, "charging_power_w": 4140, "raw": {"warp_state": {"charger_state": 3}}
  },
  "modbus": {
    "storage": {"provider": "marstek", "registers": {"32104": 580}},
    "wallbox": {"provider": "warp", "registers": {"1000": 3}},
    "another_device": {"registers": {"9999": {"words": [0, 42]}}}
  }
}
```

See [examples/telemetry-v1.json](examples/telemetry-v1.json) for a fuller example
including both heap histories, previous resets, allocation information, storage
limits, temperatures, current/voltage, phase data, and provider-specific values.
Register numbers and values in these examples are illustrative, not device
control instructions or a register specification.

Summary extraction recognizes fields in this example, plus a few common aliases:
`system.boot_id`, top-level `uptime_seconds`, `firmware_version`, `build_id`,
`sequence`, and `diagnostics.checkpoint_sequence`. The raw data never depends on
these hints being present or recognized. Numeric uptime values are interpreted
as seconds for display. Times and date filters in the admin UI use explicit UTC.

Duplicate handling is scoped to the authenticated source, in this priority order:

- `upload_id` (string or number), also scoped to `boot_id` when provided;
- `boot_id` plus `telemetry_sequence` (or top-level `sequence`);
- `boot_id` plus `checkpoint_sequence` (or `diagnostics.checkpoint_sequence`).

The sender should assign unique transmission IDs or per-boot snapshot counters.
For an identical identity, the first payload and receive timestamp win. A retry
returns `200` with `"duplicate":true`, the original receive time, and the original
persisted checkpoint acknowledgement when the retry also supplies a valid
checkpoint counter. Last seen is updated. Retries should reuse the same payload
and identifiers: a different checkpoint on a reused identity does not replace
or acknowledge unpersisted data. If either the original payload or retry has no
valid checkpoint counter, that acknowledgement field is omitted. Bare counters
without boot identity are stored normally because they may restart after a
reboot. Uploads without any identifiers are always accepted.

Responses: `200` accepted/duplicate, `400` malformed JSON/schema, `401` missing,
unknown or revoked token, `405` non-POST ingestion, `413` too large, `415` wrong
media type/unsupported content encoding, `500` actual internal failure.

## Inspect and download

The admin UI provides source/token management, a paginated upload table with
optional summary fields, formatted JSON detail, single JSON downloads, and ZIP
export filtered by source and/or dates. All UI and download routes require
administrator Basic Auth. Source names and JSON are HTML-escaped. Mutating
actions and token reveal require POST with a CSRF form token; responses are
marked `Cache-Control: no-store`.

With curl, `-u "$ADMIN_USERNAME"` prompts for the admin password:

```sh
# One snapshot, exactly as received (replace 1 with its upload ID).
curl --fail --show-error -u "$ADMIN_USERNAME" \
  "https://${TELEMETRY_HOST}/uploads/1/download" -o snapshot.json

# All snapshots for source 1, including history after token revocation.
curl --fail --show-error -u "$ADMIN_USERNAME" \
  "https://${TELEMETRY_HOST}/downloads/export.zip?source_id=1" -o source-1.zip

# Inclusive UTC dates, across all sources. Add source_id to combine filters.
curl --fail --show-error -u "$ADMIN_USERNAME" \
  "https://${TELEMETRY_HOST}/downloads/export.zip?from=2026-10-01&to=2026-10-30" -o telemetry.zip
```

Omit dates and source ID to export everything. ZIP files stream without loading
the collection into memory. Each `source-<id>/telemetry-<id>.json` contains the
**exact original request bytes**, including whitespace. Corresponding
`metadata/upload-<id>.json` sidecars contain collector upload ID, source ID/name,
and receive time. Paths use server-generated IDs, never device/source-supplied
filenames. A valid empty selection produces an empty ZIP. Failed streams are
left incomplete; a successful ZIP integrity check verifies completeness.

ZIP downloads have no fixed total-duration limit in the collector: this route
renews a 60-second write deadline for each output chunk, so a progressing export
can run substantially longer than the normal 60-second response timeout. A
blocked write remains bounded, and client disconnects cancel database iteration
and output through the request context. Other API/UI requests keep the ordinary
server timeouts. Any separately configured Traefik download timeout must also
allow the desired duration.

## Persistence and backup

On Dockyard, `./data/telemetry.db` is the database, with SQLite's associated
`telemetry.db-wal` and `telemetry.db-shm` files while running. Keep the directory
writable only by UID 65532 (or the host administrator). No retention job runs.

Database schema version 2 separates source identity from token lifecycle using
nullable encrypted token/digest and token creation/deletion timestamps. Existing
version-1 databases upgrade automatically in a single transaction on startup.
Source IDs/names, last-seen times, uploads, original JSON bytes and active tokens
are preserved. Previously revoked sources become normal sources without a
token, and their old revocation time becomes token-deletion metadata. For active
v1 tokens, token creation time is initialized from source creation time, since
v1 did not record it separately. Token creation/deletion does not modify source
creation time. Key verification runs before the migration commits; an incorrect
master key leaves the v1 schema unchanged. Keep the same encryption key and take
a consistent backup before upgrading. Telemetry payload schema remains version 1.

For an online consistent backup, install the SQLite CLI on the **host** and use
its backup operation, choosing an existing secure destination directory:

```sh
sudo sqlite3 data/telemetry.db ".backup '/secure/backups/telemetry.db'"
```

Alternatively stop the service, copy the entire data directory to a secure
backup location, then start it again:

```sh
docker compose stop hems-telemetry
sudo cp -a data /secure/backups/telemetry-data
docker compose start hems-telemetry
```

Do not copy just the main database file while the service is writing in WAL
mode. Back up the encryption key separately; the database does not contain it.
To restore, stop the service, restore a consistent backup into a clean data
directory with the same ownership, configure the matching key, and start it.
Keep the previous database separately until the restore has been verified.

## Build and test

Native development requires Go 1.26 or later, with no C compiler or SQLite
installation:

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o collector ./cmd/server
# Export ADMIN_USERNAME, ADMIN_PASSWORD, TOKEN_ENCRYPTION_KEY first.
DATA_DIR=./data ./collector
```

The direct development server listens on `:8080` over HTTP; use it locally or
behind HTTPS termination. The production Compose service exposes it only to the
Docker network. HTTP header/body/write/idle timeouts and a 16 KiB header limit
bound requests. Shutdown on SIGTERM drains active requests for up to 10 seconds.

Build an ARM64 image with a builder supporting ARM64 (native on Dockyard, or
Buildx with ARM64 emulation on another architecture):

```sh
docker build --platform linux/arm64 -t hems-telemetry:local .
```

On another architecture, an ordinary `docker build` targets that architecture.
The Dockerfile builds on the builder's native platform and cross-compiles the
static Go binary for the requested target. No test, runtime, or healthcheck
needs network access; dependency downloads happen during the build.
