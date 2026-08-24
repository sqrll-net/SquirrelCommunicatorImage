package handlers

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"sqrll.net/squirrel-communicator-image/internal/cache"
	"sqrll.net/squirrel-communicator-image/internal/sniff"
	"sqrll.net/squirrel-communicator-image/internal/storage"
)

// DownloadHandler handles GET /api/image/{hash} for file retrieval.
type DownloadHandler struct {
	Storage *storage.Manager
	Cache   *cache.Cache
}

// ServeHTTP implements http.Handler. Checks RAM cache first (RLock), falls back to disk.
func (h *DownloadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract file hash from URL path: /api/image/{hash}
	id := strings.TrimPrefix(r.URL.Path, "/api/image/")
	id = strings.TrimSuffix(id, "/")
	if !validHash(id) {
		writeError(w, "invalid file id", http.StatusBadRequest)
		return
	}

	var data []byte
	var mimeType string
	var fromCache bool

	// Hot path: RAM cache (RLock, no LRU mutation)
	if d, mt, ok := h.Cache.Get(id); ok {
		data, mimeType, fromCache = d, mt, true
	} else {
		// Cold path: read from disk and sniff the content type
		d, err := h.Storage.Read(id)
		if err != nil {
			log.Printf("Download miss: %v", err)
			writeError(w, "file not found", http.StatusNotFound)
			return
		}
		data = d
		mimeType = sniff.Detect(data)
	}

	setDownloadHeaders(w, mimeType)
	if fromCache {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)

	// Promote to hot cache after the response is sent, so eviction
	// never delays the client.
	if !fromCache {
		h.Cache.Put(id, data, mimeType)
	}
}

// setDownloadHeaders applies anti-injection headers and forces risky types to download.
func setDownloadHeaders(w http.ResponseWriter, mimeType string) {
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if mimeType == "image/svg+xml" {
		// SVG can carry active script; never render it inline in the browser.
		w.Header().Set("Content-Disposition", "attachment; filename=\"image.svg\"")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}
}

// validHash reports whether s is a 64-char lowercase/uppercase hex string.
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
