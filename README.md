# SquirrelCommunicatorImage

Content-addressable file storage microservice. Upload files, get them back by SHA-256 hash.
No database. No external dependencies. Just a filesystem and RAM.

## Security Model

- **S2S auth** — uploads require an API key (long random string). Keys are stored
  only as SHA-256 hashes and compared in constant time.
- **Per-key rate limit** — sliding one-hour window. Exceeding the limit returns
  `429` with a `Retry-After` header (the "ban" after N attempts/hour).
- **Magic-byte validation** — content is sniffed from real signatures, not from
  the client-provided filename or `Content-Type`. Disguised scripts are rejected.
- **Anti-injection headers** — every download sends `X-Content-Type-Options: nosniff`.
  SVG (which can carry `<script>`) is always forced to download with a sandboxed CSP.
- **OOM shield** — `http.MaxBytesReader` caps the body before any processing.

## Architecture

```
[ HTTP Request (from Nginx or C++) ]
                 |
                 v
+---------------------------------------------------+
| 1. MIDDLEWARE / SECURITY                          |
|    - X-SQRLL-API-KEY auth (uploads)               |
|    - per-key rate limit (sliding window)          |
|    - http.MaxBytesReader (OOM shield)             |
+---------------------------------------------------+
                 |
    +------------+------------+
    | (Method)                |
  [POST]                    [GET]
 (Upload)                 (Download)
    |                         |
    v                         v
+----------------+    +---------------------------------+
| 2. PROCESSING  |    | 3. HOT PATH (RAM Cache)         |
| - Read to RAM  |    | - sync.RWMutex.RLock()          |
| - Magic bytes  |    | - map[string][]byte lookup      |
| - SHA-256 hash |    |                                 |
+----------------+    +---------+-----------+           |
    |                           |           |           |
    v                         [HIT]       [MISS]        |
+----------------+              |           |           |
| 4. DEDUP       |              |           v           |
| - os.Stat()    |              |   +-----------------+ |
|   If exists:   |              |   | 5. COLD PATH    | |
|   RETURN ID    |              |   | - os.ReadFile() | |
+----------------+              |   | - sniff MIME    | |
    | (new file)                |   +-----------------+ |
    v                           |           |           |
+----------------+              |           v           |
| 6. COLD WRITE  |              |   +-----------------+ |
| - tmp+rename   |              |   | 6. PROMOTION    | |
| - atomic.Add() |              |   | - Lock()        | |
| (quota check)  |              |   | - Add to RAM    | |
+----------------+              |   | - Evict if full | |
    |                           |   +-----------------+ |
    v                           v           v
[ JSON: {id, status, type, size} ] [ Raw bytes + safe headers ]
```

### Boot Sequence

On startup, `filepath.WalkDir` scans the storage directory, rebuilds the in-memory
extension map (`hash -> .ext`), and sums all file sizes into an `atomic.Int64` quota
counter. No database sync needed. Takes milliseconds even for thousands of files.

### Cache Design

Size-bounded (1GB default), not item-count-bounded. A 10KB icon and an 8MB video
are treated fairly — the cache tracks total bytes, not file count.

Reads use `RLock` only — no LRU mutation on Get. This means 500 concurrent requests
for the same file all proceed in parallel without contention. The tradeoff is that
eviction order is insertion order (FIFO), not true LRU.

### Deduplication

Files are addressed by SHA-256 content hash. Upload the same image twice, and the
second upload returns immediately with `"status": "duplicate"` — no disk write,
no quota hit.

### Write Safety

New files are written to a temp file with a random suffix, then atomically renamed
into place. Crash at any point, and you either have the complete file or nothing.

## Authentication & Key Management

Keys are long random strings (64 hex chars, up to 1024). Only their SHA-256 hashes
are kept in memory — the plaintext is never stored or logged.

**Login (register a key)** — admin endpoint, requires the master key:

```
POST /api/key
Headers: X-SQRLL-IMAGE-API-KEY: <master>

Response 201:
  { "key": "f3b0c44298fc1c149afbf4c8996fb924..." }
```

**Logout (revoke a key)** — admin endpoint, requires the master key and the key to revoke:

```
DELETE /api/key
Headers: X-SQRLL-IMAGE-API-KEY: <master>
         X-SQRLL-API-KEY: <key-to-revoke>

Response 200:
  { "status": "revoked" }
```

Bootstrap keys can be preloaded at startup via `SQRLL_AUTH_KEYS` (comma-separated).

## Endpoints

### POST /api/image/upload

Upload a file. Requires a registered API key.

```
Request:
  POST /api/image/upload
  Headers: X-SQRLL-API-KEY: <key>
  Body: raw file bytes

Response 201 (new file):
  {
    "id": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "status": "ok",
    "type": "image/png",
    "size": 1024
  }

Response 200 (duplicate):
  { "id": "...", "status": "duplicate", "type": "image/png", "size": 1024 }

Errors:
  403 - Missing/invalid API key
  413 - Body exceeds max upload size
  415 - Unsupported file type
  429 - Rate limit exceeded (with Retry-After)
  500 - Storage write failure
```

### GET /api/image/{sha256-hash}

Download a file by its SHA-256 hash. Public (no key required).

```
Request:
  GET /api/image/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855

Response 200:
  Headers:
    Content-Type: image/png
    Content-Length: 1024
    X-Cache: HIT | MISS
    X-Content-Type-Options: nosniff
    (SVG only) Content-Disposition: attachment
  Body: raw file bytes

Errors:
  400 - Missing or invalid file ID
  404 - File not found
```

### GIF Provider Proxy

Proxy for KLIPY GIF search/trending and server-side GIF fetch-for-re-upload. The
KLIPY key is attached server-side and never returned to the browser.

All three endpoints return JSON, support `OPTIONS` preflight, and require auth
(the same API key issued on login, sent via `X-SQRLL-API-KEY` or `X-API-Token`).

#### GET /api/gifs/search

```
Query: q (required, trimmed non-empty), limit (1..50, default 25), page (1..1000, default 1)
Forward: https://api.klipy.com/api/v1/{KEY}/gifs/search?q=...&per_page={limit}&page={page}&content_filter=off

Response 200:
  { "results": [ ... ], "has_next": true }
```

#### GET /api/gifs/trending

```
Query: limit (1..50, default 25), page (1..1000, default 1)
Forward: https://api.klipy.com/api/v1/{KEY}/gifs/trending?per_page={limit}&page={page}&content_filter=off

Response 200:
  { "results": [ ... ], "has_next": true }
```

#### GET /api/gifs/fetch

Downloads a remote GIF server-side and re-uploads it into content-addressable
storage, returning the same upload response as `/api/image/upload`.

```
Query: url (required) — remote GIF to download for re-upload

Response 201:
  { "id": "<sha256>", "status": "ok", "type": "image/gif", "size": 12345 }

Guards:
  - SSRF guard (loopback/private/link-local/multicast/unspecified hosts rejected)
  - http/https scheme only, redirects disabled
  - 8 MB download cap, ~25s upstream timeout
  - magic-byte content validation (same as uploads)

Errors:
  400 - Missing/invalid url, or non-public host
  401 - Missing/invalid API key
  413 - Remote content exceeds 8 MB
  415 - Remote content is not an allowed type
  429 - Rate limit exceeded (with Retry-After)
  502 - Upstream transport error / non-200 / malformed JSON
  503 - KLIPY key unset
```

#### Normalized result item

```json
{
  "id": 123, "slug": "...", "title": "...",
  "preview": {"url": "...", "width": 0, "height": 0},
  "gif":     {"url": "...", "width": 0, "height": 0},
  "mp4":     {"url": "...", "width": 0, "height": 0}
}
```

Rendition selection rules: ads are skipped; items without a usable GIF URL are
skipped; preview prefers static formats (webp → jpg → png → gif) from sizes
sm → md → xs → hd; gif and mp4 are picked from sizes md → sm → hd → xs.

### GET /health

```
Response 200:
  {"status": "ok"}
```

## Environment Variables

| Variable                   | Default                  | Description                                         |
|----------------------------|--------------------------|-----------------------------------------------------|
| `STORAGE_PATH`             | `/var/data/sqrll/media`  | File storage directory                              |
| `SQRLL_IMAGE_SERVICE_URL`  | (empty = all interfaces) | Address to bind the HTTP server to                 |
| `SQRLL_IMAGE_PORT`         | `8083`                   | HTTP listen port                                    |
| `SQRLL_IMAGE_API_KEY`      | (empty = disabled)       | Admin key for key management endpoints              |
| `SQRLL_AUTH_KEYS`          | (empty)                  | Comma-separated bootstrap API keys                  |
| `SQRLL_KLIPY_API_KEY`      | (empty = disabled)       | KLIPY app key for GIF endpoints (503 if empty)     |
| `MAX_REQUESTS_PER_HOUR`    | `100`                    | Per-key rate limit (hourly window)                  |
| `MAX_DISK_GB`              | `100`                    | Disk quota in GB                                    |
| `MAX_RAM_MB`               | `1024`                   | RAM cache limit in MB                               |
| `MAX_UPLOAD_MB`            | `8`                      | Max single upload size                              |

## Allowed File Types

| Category | MIME Types                                              |
|----------|---------------------------------------------------------|
| Images   | image/jpeg, image/png, image/gif, image/webp,           |
|          | image/svg+xml, image/bmp, image/tiff                    |
| Video    | video/mp4, video/webm, video/ogg                        |
| Audio    | audio/mpeg, audio/ogg, audio/wav, audio/webm            |
| Document | application/pdf                                         |

Ogg and WebM are generic containers; they are labeled as `video/*` even when they
carry audio-only streams. This does not affect upload acceptance.

## Project Structure

```
sqrll-go-files/
├── cmd/
│   └── api/
│       └── main.go          Entry point, wires components, starts HTTP server
├── internal/
│   ├── auth/
│   │   └── manager.go       API key registry (hashed) + per-key rate limiting
│   ├── cache/
│   │   └── lru.go           RAM cache with size-bounded eviction
│   ├── config/
│   │   └── env.go           Environment variable loading and defaults
│   ├── handlers/
│   │   ├── upload.go        POST handler: auth, rate limit, sniff, SHA-256, dedup
│   │   ├── download.go      GET handler: cache-first, disk fallback, safe headers
│   │   ├── keys.go          Key management endpoints (login/logout)
│   │   └── gifs.go          KLIPY GIF proxy: search/trending/fetch
│   ├── sniff/
│   │   └── sniff.go         Magic-byte content detection
│   └── storage/
│       └── manager.go       Disk operations: boot scan, quota, atomic writes
├── go.mod
└── Dockerfile               Multi-stage: golang:1.21-alpine -> scratch
```

## Build & Run

```bash
go build -o server ./cmd/api
./server
```

### Docker

```bash
docker build -t sqrll-image .
docker run -p 8083:8083 -v /data/media:/var/data/sqrll/media \
  -e SQRLL_IMAGE_API_KEY=... -e SQRLL_AUTH_KEYS=... -e SQRLL_KLIPY_API_KEY=... sqrll-image
```

If you change `SQRLL_IMAGE_PORT` inside the container, the `-p` mapping must match
it (e.g. `SQRLL_IMAGE_PORT=9090` requires `-p 9090:9090`).

## Key Properties

- **Stateless.** Restart the container and it rebuilds all state from disk in under a second.
- **Self-healing.** Boot scan recovers the full index. Empty cache, full disk state — ready immediately.
- **Dogpile-safe.** 500 concurrent requests for the same file: 1 disk read, 499 RAM hits. All under RLock.
- **Horizontally scalable.** No shared state between instances. Spin up another container with its own volume.
  (Note: per-key rate limits are in-memory and therefore per-instance.)
- **Zero dependencies.** Standard library only. No CGO. Minimal attack surface.
