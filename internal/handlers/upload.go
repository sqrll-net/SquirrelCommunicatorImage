package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"sqrll.net/squirrel-communicator-image/internal/auth"
	"sqrll.net/squirrel-communicator-image/internal/cache"
	"sqrll.net/squirrel-communicator-image/internal/config"
	"sqrll.net/squirrel-communicator-image/internal/sniff"
	"sqrll.net/squirrel-communicator-image/internal/storage"
)

// UploadHandler handles POST /api/image/upload for file ingestion.
type UploadHandler struct {
	Storage  *storage.Manager
	Cache    *cache.Cache
	Auth     *auth.Manager
	MaxBytes int64
}

// uploadResponse is the JSON payload returned after a successful upload.
type uploadResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
}

// errorResponse is the JSON payload returned on any error.
type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// ServeHTTP implements http.Handler with auth, rate limiting, content
// validation, dedup, and storage.
func (h *UploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	apiKey := r.Header.Get("X-SQRLL-API-KEY")

	// Authenticate the key (constant-time hash lookup)
	if !h.Auth.Authenticate(apiKey) {
		writeError(w, "forbidden", http.StatusForbidden)
		return
	}

	// Per-key rate limit (sliding window)
	if ok, retry := h.Auth.Allow(apiKey); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// OOM shield: limit body size
	r.Body = http.MaxBytesReader(w, r.Body, h.MaxBytes)
	defer func() { _ = r.Body.Close() }()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeError(w, "failed to read body", http.StatusBadRequest)
		return
	}

	if len(data) == 0 {
		writeError(w, "empty body", http.StatusBadRequest)
		return
	}

	// Content validation via magic-byte sniffing
	mimeType := sniff.Detect(data)
	if !config.AllowedMIMETypes[mimeType] {
		log.Printf("Rejected upload: detected MIME %s is not allowed", mimeType)
		writeError(w, "unsupported file type", http.StatusUnsupportedMediaType)
		return
	}

	// SHA-256 content hash
	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])

	// Dedup: file already on disk
	if h.Storage.Exists(hashHex) {
		h.Cache.Put(hashHex, data, mimeType)
		writeJSON(w, http.StatusOK, uploadResponse{
			ID:     hashHex,
			Status: "duplicate",
			Type:   mimeType,
			Size:   int64(len(data)),
		})
		return
	}

	// Write to disk
	if err := h.Storage.Write(hashHex, data, mimeType); err != nil {
		log.Printf("Write error: %v", err)
		writeError(w, "storage write failed", http.StatusInternalServerError)
		return
	}

	// Promote to hot cache
	h.Cache.Put(hashHex, data, mimeType)

	writeJSON(w, http.StatusCreated, uploadResponse{
		ID:     hashHex,
		Status: "ok",
		Type:   mimeType,
		Size:   int64(len(data)),
	})
}

// writeJSON serializes a struct to JSON and writes it to the response.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a JSON error response.
func writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg, Code: code})
}
