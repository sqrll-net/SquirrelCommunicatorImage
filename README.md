# SquirrelCommunicatorImage

Content-addressable file storage microservice. Upload files, get them back by SHA-256 hash.
No database. No external dependencies. Just a filesystem and RAM.

## Architecture

```
[ HTTP Request (from Nginx or C++) ]
                 |
                 v
+---------------------------------------------------+
| 1. MIDDLEWARE / SECURITY                          |
|    - X-SQRLL-API-KEY header check (uploads only)  |
|    - http.MaxBytesReader (OOM shield, 8MB cap)    |
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
| - Detect MIME  |    | - map[string][]byte lookup      |
| - SHA-256 hash |    |                                 |
+----------------+    +---------+-----------+           |
    |                           |           |           |
    v                         [HIT]       [MISS]        |
+----------------+              |           |           |
| 4. DEDUP       |              |           v           |
| - os.Stat()    |              |   +-----------------+ |
|   If exists:   |              |   | 5. COLD PATH    | |
|   RETURN ID    |              |   | - os.ReadFile() | |
+----------------+              |   | - From Disk     | |
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
[ JSON: {id, status, size} ]  [ Raw bytes + Content-Type ]
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
no quota hit. A simple `os.Stat()` check replaces a database lookup.

### Write Safety

New files are written to a temp file with a random suffix, then atomically renamed
into place. Crash at any point, and you either have the complete file or nothing.
No partial writes on disk. Ever.

## Endpoints

### POST /api/image/upload

Upload a file. Requires `X-SQRLL-API-KEY` header if configured.

```
Request:
  POST /api/image/upload
  Headers: X-SQRLL-API-KEY: <key>
  Body: raw file bytes

Response 201 (new file):
  {
    "id": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "status": "ok",
    "size": 1024
  }

Response 200 (duplicate):
  {
    "id": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "status": "duplicate",
    "size": 1024
  }

Errors:
  403 - Missing or invalid API key
  413 - Body exceeds max upload size
  415 - Unsupported file type
  500 - Storage write failure
```

### GET /api/image/{sha256-hash}

Download a file by its SHA-256 hash.

```
Request:
  GET /api/image/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855

Response 200:
  Headers:
    Content-Type: image/png
    Content-Length: 1024
    X-Cache: HIT | MISS
  Body: raw file bytes

Errors:
  400 - Missing file ID in path
  404 - File not found
```

### GET /health

```
Response 200:
  {"status": "ok"}
```

## Environment Variables

| Variable            | Default                   | Description                  |
|---------------------|---------------------------|------------------------------|
| `STORAGE_PATH`      | `/var/lib/sqrll/media`    | File storage directory       |
| `SQRLL_IMAGE_API_KEY` | (empty = no auth)       | API key for upload endpoint  |
| `MAX_DISK_GB`       | `200`                     | Disk quota in GB             |
| `MAX_RAM_MB`        | `1024`                    | RAM cache limit in MB        |
| `MAX_UPLOAD_MB`     | `8`                       | Max single upload size       |
| `PORT`              | `8083`                    | HTTP listen port             |

## Allowed File Types

| Category | MIME Types                                              |
|----------|---------------------------------------------------------|
| Images   | image/jpeg, image/png, image/gif, image/webp,          |
|          | image/svg+xml, image/bmp, image/tiff                    |
| Video    | video/mp4, video/webm, video/ogg                        |
| Audio    | audio/mpeg, audio/ogg, audio/wav, audio/webm            |
| Document | application/pdf                                         |

## Project Structure

```
sqrll-go-files/
├── cmd/
│   └── api/
│       └── main.go          Entry point, wires components, starts HTTP server
├── internal/
│   ├── cache/
│   │   └── lru.go           RAM cache with size-bounded eviction
│   ├── config/
│   │   └── env.go           Environment variable loading and defaults
│   ├── handlers/
│   │   ├── upload.go        POST handler: auth, MIME check, SHA-256, dedup
│   │   └── download.go      GET handler: cache-first, disk fallback
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
docker run -p 8083:8083 -v /data/media:/var/lib/sqrll/media sqrll-image
```

## Key Properties

- **Stateless.** Restart the container and it rebuilds all state from disk in under a second. No database to sync.
- **Self-healing.** Boot scan recovers the full index. Empty cache, full disk state — ready immediately.
- **Dogpile-safe.** 500 concurrent requests for the same file: 1 disk read, 499 RAM hits. All under RLock.
- **Horizontally scalable.** No shared state between instances. Spin up another container with its own volume.
- **Zero dependencies.** Standard library only. No CGO. Minimal attack surface.
