# OpenDTU-HEMS telemetry collector

A Go collector and monitoring dashboard for remote OpenDTU-HEMS installations.
Devices send periodic JSON snapshots over HTTPS with a sender-specific Bearer
token. The collector preserves the original bytes in SQLite and turns normalized
HEMS observations into energy, tariff, optimizer, device and ESP32 health views.
It observes installations; it never controls devices or automatically deletes
snapshots.

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
driver (`modernc.org/sqlite`, the only direct external dependency). HTML templates, application assets and the locally bundled
Apache ECharts 5.6.0 library are embedded in the binary. There is no production
Node runtime, CDN or SPA framework. The runtime image is a static binary in `scratch`,
running as UID/GID **65532:65532**. It supports Linux ARM64 without CGO.

SQLite keeps authoritative records in `telemetry_sources` and `telemetry_uploads`,
and a rebuildable dashboard projection in `dashboard_snapshots`. Uploads retain
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

## Create the first sender and send a snapshot

1. Open `https://<your-host>/sources` and supply the configured administrator
   credentials when the browser prompts.
2. Enter a name such as **House** or **Garage** and select **Create sender**. The sender
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

## Monitoring dashboard

[Visual review: dark, light and mobile screenshots](docs/dashboard/README.md)

Open `/overview` (also the landing page at `/`). The sidebar links to Overview,
Live View, HEMS, Runtime, Devices, Uploads, Senders / Tokens and Downloads / Exports.
All pages, assets and dashboard JSON endpoints require administrator Basic Auth.
Source remains the database/backend term; the UI calls that identity a **Sender**.

- **Overview:** choose a sender by name and Last 6h / 24h / 7d / 30d, or a custom
  UTC date/time range. “Current” always means the newest **capture inside the
  selected range**, including the current HEMS decision. Empty ranges have no
  current values. Refresh loads a new dataset; it does not change custom dates.
- **Live View:** the newest **received** snapshot for that sender, independent of
  historical ranges. Polls every 15 seconds while visible and refreshes on return
  to the tab. A delayed retry can therefore show an old capture with a new receive
  time. This is snapshot-live, with no invented measurements between snapshots.
- **HEMS:** plan validity/revision, readiness/blockers, Monitor-mode Active preview,
  economic assumptions, costs, constraints and an explicitly modeled schedule.
  Invalid/cached plans remain inspectable with validity shown. The collector does
  not manufacture an action if `shadow_if_active.valid` is false.
- **Runtime:** INTERNAL free/largest/minimum heap and PSRAM on separate axes, reset,
  uptime, allocation/expired/dropped diagnostics, task stack watermarks, payload
  size and transmission/checkpoint sequence details when present.
- **Devices:** storage, EVSE, tariff/forecast provider status, readiness, poll errors,
  ages and a recent capture-quality history. The firmware payload does not include
  every remote sender service/retry counter; unavailable state stays unavailable.

Filters are kept in the URL (`sender`, `range`, `from`, `to`) and carried between
monitoring views. Custom boundaries are RFC3339 UTC instants: inclusive `from`,
exclusive `to`. Browser Back/Forward restores the view. Dashboard filters use
**`hems.captured_at_utc`**, which current firmware sends as Unix seconds. Legacy
records without a valid device capture clock use collector receive time with an
explicit `collector_receive_fallback` marker. Receive time remains visible in
chart tooltips and raw upload details. Raw Uploads/ZIP export date filters retain
their existing **receive-time**, inclusive UTC calendar-day behavior.

Cards show capture age. Snapshot freshness is separate from the firmware's
`fresh` / `stale` / `unknown` per-signal quality. Cadence is explicitly estimated
from the median of up to 31 recent distinct capture intervals for that sender;
more than twice that cadence plus 60 seconds is stale. With insufficient history,
cadence/freshness is unknown and values are labeled last known. Irregular event
triggers can affect the estimate. Firmware-stale values remain labeled stale;
null/unknown values display **— / Unavailable**, never a genuine zero.

Grid positive means import, negative export; storage positive means charge,
negative discharge; PV positive means production and EVSE positive consumption.
Market price and effective import price are separate, divided by **10,000** from
micros/kWh into ct/kWh; the tariff currency is retained in HEMS details. Modeled
nano-currency costs are divided by 1e9. No mockup values are used as real data.

Charts share axes, colors, tooltips, capture-time zoom and a locally bundled
Apache ECharts 5.6.0 renderer. Unknown signals break the line. Gaps longer than the
estimated stale threshold also break the line. Only captured current tariff
intervals enter historical price lines; the optimizer schedule is separate and
labeled planned. Chart legends toggle series. Raw values and detailed records
remain available through the recent-capture table and Uploads. The API returns at
most the newest **10,000** captures per range with an explicit truncation notice;
statistics then cover the displayed subset. Narrow the range to inspect more.
Devices history shows the newest 100 captures in the selected dataset.

The theme initially follows the system preference. The theme button selects and
locally persists an explicit dark/light override. Clear the `hems-theme` local
storage key to follow the system again. Both themes use shared CSS tokens;
responsive layouts stack analytical panels on small screens. Charts have text
summaries, labeled controls, visible keyboard focus and tabular raw alternatives.

Dashboard JSON routes (compact allowlisted data, never full raw payloads):

| Route | Semantics |
| --- | --- |
| `GET /dashboard/data?sender=1&range=24h` | Capture-filtered points and newest filtered details |
| `GET /dashboard/data?sender=1&range=custom&from=2026-10-30T00:00:00Z&to=2026-10-31T00:00:00Z` | Half-open custom capture range |
| `GET /dashboard/live?sender=1` | Newest received snapshot, historical filters ignored |

The schema projection is based on OpenDTU-HEMS PR #38, commit
`998620bbfe485561dc6d745091b158a0c01225f6`. Collector-side projection/UI improvements
can be shipped without changing firmware; unknown future fields remain in the
original JSON.

## Inspect and download

The admin UI also preserves sender/token management, a paginated upload table with
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

Database schema **version 3** adds `dashboard_snapshots`, one derived row per
accepted upload, with capture-time and receive-order indexes scoped by source.
It stores an allowlisted point projection and a compact latest-detail/schedule
projection, with a projection version. Existing v1/v2 databases are backfilled
from raw payloads transactionally on first startup; allow time for this one-time
scan on large databases. Each new snapshot is projected within the same acceptance
transaction. Deduplicated retries do not replace the original bytes or projection.
Dashboard reads do not repeatedly parse raw payloads. No time-series service is
needed. The raw JSON BLOB remains authoritative and all original unknown fields,
IDs, tokens, ACK/dedupe identity and exports remain unchanged.

To rebuild all derived dashboard values from raw JSON, stop the collector and run
`/collector rebuild-dashboard` with the same `DATA_DIR`, `TOKEN_ENCRYPTION_KEY`
and admin environment as the service, then restart it. For Compose:

```sh
docker compose stop hems-telemetry
docker compose run --rm hems-telemetry rebuild-dashboard
docker compose up -d hems-telemetry
```

Rebuilding is transactional and does not alter telemetry or token lifecycle.
The Go API `Store.RebuildDashboard` implements the same operation. Future changes
to projection format must increment its version and provide a migration/backfill.
Static scratch-image and ARM64/CGO-free deployment remain unchanged; Node is only
an optional development check dependency.

Database schema version 2 separates source identity from token lifecycle using
nullable encrypted token/digest and token creation/deletion timestamps. Existing
version-1 databases also upgrade automatically in a single transaction on startup.
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

## Dashboard development and visual checks

```sh
go test ./...
go vet ./...
go build -o collector ./cmd/server
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/collector-arm64 ./cmd/server
npm ci
npm run check
# In another terminal, from the repository root:
go run ./scripts/preview
# Disposable synthetic data only, localhost:8081, Basic Auth preview / preview:
npm run test:browser
```

Browser checks use locally installed Chromium (`CHROMIUM_PATH` overrides
`/usr/bin/chromium`) through Playwright Core. Screenshots are written to
`/tmp/hems-dashboard-artifacts` (override `DASHBOARD_ARTIFACTS`). The synthetic
preview uses its own temporary database and no production credentials. Its
fixture mirrors firmware serialization, including nullable normalized signals;
it is not installation data. The production image does not include Node, the
preview command, fixtures as UI data, or browser tooling.
