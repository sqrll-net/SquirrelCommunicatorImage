package handlers

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sqrll-net/squirrel-communicator-image/internal/cache"
	"github.com/sqrll-net/squirrel-communicator-image/internal/storage"
)

/** DownloadHandler handles GET /images/{id} for file retrieval. */
type DownloadHandler struct {
	Storage *storage.Manager
	Cache   *cache.Cache
}

/** ServeHTTP implements http.Handler. Checks RAM cache first (RLock), falls back to disk. */
func (h *DownloadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract file ID from URL path: /images/{id}
	id := strings.TrimPrefix(r.URL.Path, "/images/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		writeError(w, "missing file id", http.StatusBadRequest)
		return
	}

	// Hot path: check RAM cache (RLock, no LRU mutation)
	if data, ok := h.Cache.Get(id); ok {
		w.Header().Set("Content-Type", http.DetectContentType(data))
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	// Cold path: read from disk
	data, mimeType, err := h.Storage.Read(id)
	if err != nil {
		log.Printf("Download miss: %v", err)
		writeError(w, "file not found", http.StatusNotFound)
		return
	}

	// Promote to hot cache (Lock, may trigger eviction)
	h.Cache.Put(id, data)

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}
