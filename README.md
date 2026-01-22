# IEEE Office Backend

Simple Go HTTP service to track office attendance using RFID/UID scans (designed for ESP32 or similar devices). It records sign-in and sign-out events, keeps an in-memory list of current attendees, and persists visits to a local SQLite database.

Can be used by the [IEEE Office Scanner ESP32](https://github.com/ieee-uottawa/ieee-office-scanner-esp32) device and the [IEEE Office Discord Bot](https://github.com/ieee-uottawa/ieee-office-discord-bot) to provide office presence tracking.

## Features

- **Scan endpoint**: Accepts POSTed UID payloads from an RFID reader to toggle sign-in / sign-out.
- **Current attendees**: Returns who is currently in the room and when they signed in.
- **Visits management**: Retrieve, filter, and delete completed visits (signin + signout) stored in SQLite via API.
- **Scan history**: Keeps the last 10 scans in memory (uid + timestamp) and exposes them via an API.
- **Members management**: Create/list registered members (UID, name, discord_id) via API; import/export members with JSON file.
- **Discord sign-in/out**: Sign in or out by providing a member's `discord_id`.
- **Nightly cleanup**: Force sign-out of all active attendees at 4:00 AM local time.
- **Persistent store**: Uses `data/attendance.db` (SQLite) and persists active attendees to `data/current_attendees.json`.
- **CORS support**: Configurable cross-origin resource sharing for web-based frontends.

## Files of interest

- `main.go` — application source with HTTP handlers for `/scan`, `/current`, `/visits`, and `/members`.
- `Dockerfile` — multi-stage build for producing a small runtime container.
- `docker-compose.yml` — convenient compose file to run the service locally.
- `data/` — folder for runtime files: `members.json`, `current_attendees.json`, `attendance.db`.

## Requirements

- Go toolchain (tested with Go 1.25). The project uses modules — `go.mod` is present.
- Docker (optional) to build and run the containerized image.

## Quick Start — Run locally

- Run directly (development):

```bash
go run .
```

- Build a local binary and run:

```bash
go build -o attendance .
./attendance
```

The server listens on `:8080` by default. Set the environmental variables as needed (see Configuration below).

## Configuration

The server can be configured using environment variables:

- `ALLOWED_ORIGINS` - CORS allowed origins (default: `*` for all origins)
  - Set to specific origins for production: `ALLOWED_ORIGINS=https://yourdomain.com`
  - Use comma-separated list for multiple origins: `ALLOWED_ORIGINS=http://localhost:3000,https://yourdomain.com`
- `SCANNER_API_KEY` - API key for ESP32 scanner (optional, enables authentication)
- `DISCORD_BOT_API_KEY` - API key for Discord bot (optional, enables authentication)
- `API_KEYS` - Comma-separated list of additional API keys (optional)

You can set them using a `.env` file and a tool like `direnv` or `dotenv`, or export them in your shell before running the server (e.g., `export SCANNER_API_KEY=yourkey`). The Docker Compose setup automatically loads from `.env`.

**Security Note**: If any API key is configured, all endpoints (except `/health`) require the `X-API-Key` header. See [SECURITY.md](SECURITY.md) for detailed setup instructions.

Copy `.env.example` to `.env` and customize as needed.

## Using Docker

- Build the image locally:

```bash
docker build -t ieee-office-backend:latest .
```

- Run container (mount `./data` so records persist on the host):

```bash
docker run --rm -p 8080:8080 -v "$(pwd)/data:/data" ieee-office-backend:latest
```

## Using Docker Compose

- Start service with compose (the included `docker-compose.yml` mounts `./data` into the container at `/data`):

```bash
docker compose up --build
```

Persistent Data & File Layout

- `data/members.json` — used by the export/import endpoints. Expected format: a JSON array of members, each with `name`, `uid`, and `discord_id`. Example:

```json
[
    { "name": "Alice", "uid": "UID_ABC_123", "discord_id": "111111111" },
    { "name": "Bob",   "uid": "UID_XYZ_456", "discord_id": "222222222" }
]
```

- `data/current_attendees.json` — automatically written/loaded by the server to track currently signed-in UIDs.
- `data/attendance.db` — SQLite DB file created by the app to store members and visits.

## HTTP API

- `POST /scan` — body: `{ "uid": "<UID string>" }`. The server will:
      - Return `status: "in"` on successful sign-in.
      - Return `status: "out"` on sign-out and persist a visit to the DB.
      - Unknown UID returns HTTP `403 Forbidden`.

Example:

```bash
curl -X POST http://localhost:8080/scan -H 'Content-Type: application/json' \
    -d '{"uid":"UID_ABC_123"}'
```

- `GET /scan_history` — returns the last 10 scans (newest first). Each item has `uid` and `time` (RFC3339).

```bash
curl http://localhost:8080/scan_history
```

- `GET /current` — returns JSON array of currently signed-in users (name + signin_time).

```bash
curl http://localhost:8080/current
```

- `GET /visits` — returns visits (name, signin_time, signout_time). Supports optional query parameters for filtering:
  - `from` - RFC3339 formatted start date (inclusive) to filter visits from this date onwards
  - `to` - RFC3339 formatted end date (inclusive) to filter visits up to this date
  - `member_id` - filter visits by specific member ID
  - `limit` - maximum number of records to return (newest first)
  - `format` - output format: `json` (default) or `csv` for CSV file download

```bash
# Get all history as JSON (default)
curl http://localhost:8080/visits

# Get history from a specific date
curl "http://localhost:8080/visits?from=2024-01-01T00:00:00Z"

# Get history within a date range
curl "http://localhost:8080/visits?from=2024-01-01T00:00:00Z&to=2024-12-31T23:59:59Z"

# Get the 10 most recent visits
curl "http://localhost:8080/visits?limit=10"

# Get all visits for a specific member
curl "http://localhost:8080/visits?member_id=5"

# Combine filters: get 50 most recent visits from member 5 in January 2024
curl "http://localhost:8080/visits?member_id=5&from=2024-01-01T00:00:00Z&to=2024-01-31T23:59:59Z&limit=50"

# Export all visits as CSV file
curl "http://localhost:8080/visits?format=csv" -o visits.csv

# Export filtered visits as CSV (all filters work with CSV format)
curl "http://localhost:8080/visits?format=csv&from=2024-01-01T00:00:00Z&to=2024-12-31T23:59:59Z" -o visits.csv
```

The CSV export includes columns: Name, Sign In Time, Sign Out Time, and Duration.

- `DELETE /visits` — delete visits based on filters. Requires at least one filter (`from`, `to`, or `member_id`) to prevent accidental deletion of all visits.
  - `from` - RFC3339 formatted start date to delete visits from this date onwards
  - `to` - RFC3339 formatted end date to delete visits up to this date
  - `member_id` - delete visits for a specific member ID

```bash
# Delete all visits from January 2024 onwards
curl -X DELETE "http://localhost:8080/visits?from=2024-01-01T00:00:00Z"

# Delete all visits up to December 2023
curl -X DELETE "http://localhost:8080/visits?to=2023-12-31T23:59:59Z"

# Delete visits within a specific date range
curl -X DELETE "http://localhost:8080/visits?from=2024-01-01T00:00:00Z&to=2024-01-31T23:59:59Z"

# Delete all visits for a specific member
curl -X DELETE "http://localhost:8080/visits?member_id=5"

# Delete visits for a specific member within a date range
curl -X DELETE "http://localhost:8080/visits?member_id=5&from=2024-01-01T00:00:00Z&to=2024-01-31T23:59:59Z"
```

Returns the number of visits deleted. Returns `400` if no filters are provided.

- `GET /members` — returns registered members stored in the DB.

```bash
curl http://localhost:8080/members
```

- `POST /members` — create a new member. Body: `{ "name": "Charlie", "uid": "UID_123", "discord_id": "333333333" }`.

```bash
curl -X POST http://localhost:8080/members -H 'Content-Type: application/json' \
    -d '{"name":"Charlie","uid":"UID_123","discord_id":"333333333"}'
```

- `PUT /members/{id}` — update an existing member by ID. Body: `{ "name": "Charlie Updated", "uid": "UID_123", "discord_id": "333333333" }`.

```bash
curl -X PUT http://localhost:8080/members/1 -H 'Content-Type: application/json' \
    -d '{"name":"Charlie Updated","uid":"UID_123","discord_id":"333333333"}'
```

Returns the updated member on success, `404` if member not found, or `409` if the UID conflicts with another member.

- `DELETE /members/{id}` — delete an existing member by ID.

```bash
curl -X DELETE http://localhost:8080/members/1
```

Returns a success message on deletion, `404` if member not found, or `409` if the member is currently signed in. Note: Deleting a member will cascade delete all associated visit history.

- `GET /count` — returns the count of currently signed-in attendees.

```bash
curl http://localhost:8080/count
```

Response: `{"count": 3}`

- `GET /health` — health check endpoint that returns `200 OK` with "OK" text response.

```bash
curl http://localhost:8080/health
```

- `POST /signout_all` — signs out all currently signed-in attendees. Returns a message with the count of people signed out.

```bash
curl -X POST http://localhost:8080/signout_all
```

Response: `{"message": "Signed out all attendees (3 total)."}`

- `POST /signin_discord` — sign in a member by Discord ID. Body: `{ "discord_id": "111111111" }`.

```bash
curl -X POST http://localhost:8080/signin_discord -H 'Content-Type: application/json' \
    -d '{"discord_id":"111111111"}'
```

- `POST /signout_discord` — sign out a member by Discord ID. Body: `{ "discord_id": "111111111" }`.

```bash
curl -X POST http://localhost:8080/signout_discord -H 'Content-Type: application/json' \
    -d '{"discord_id":"111111111"}'
```

- `GET /export_members` — export all members to `data/members.json`.

```bash
curl http://localhost:8080/export_members
```

- `POST /import_members` — import members from `data/members.json` into the database (existing UIDs are ignored).

```bash
curl -X POST http://localhost:8080/import_members
```

## Testing

- Unit tests are included, run them with:

```bash
# Run all tests
go test ./...

# Run with verbose output
go test -v ./...

# Run with race detector (recommended)
go test -race ./...

# Run specific test category
go test -v -run TestAPIKey  # API key authentication tests
go test -v -run TestHandle  # Handler tests
```

### Test Coverage

- **Handler tests**: All HTTP endpoints (scan, members, history, etc.)
- **API key authentication**: Middleware validation, multiple keys, environment loading
- **Integration tests**: End-to-end workflows with authentication
- **Edge cases**: Invalid input, missing data, error conditions

All tests use in-memory SQLite databases for speed and isolation.

## Deployment notes

- Keep the `data/` folder mounted on persistent storage when running in containers so the SQLite DB and current attendees are not lost between restarts.
- **Security**: Configure API keys for production deployment (see [SECURITY.md](SECURITY.md))

## Implementation Notes

- Concurrency: shared in-memory maps are protected by an `RWMutex`. File I/O and DB operations are performed outside of locks where possible to avoid blocking.
- Nightly cleanup at 4:00 AM clears active attendees. Sign out times are set to 4:00 AM for those visits.
