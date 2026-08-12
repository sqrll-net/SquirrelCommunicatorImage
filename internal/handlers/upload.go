package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/sqrll-net/squirrel-communicator-image/internal/cache"
	"github.com/sqrll-net/squirrel-communicator-image/internal/config"
	"github.com/sqrll-net/squirrel-communicator-image/internal/storage"
)

/** UploadHandler handles POST /api/internal/upload for file ingestion. */
type UploadHandler struct {
	Storage  *storage.Manager
	Cache    *cache.Cache
	APIKey   string
	MaxBytes int64
}

/** uploadResponse is the JSON payload returned after a successful upload. */
type uploadResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Size   int64  `json:"size"`
}

/** errorResponse is the JSON payload returned on any error. */
type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

/** ServeHTTP implements http.Handler with security checks, MIME validation, dedup, and storage. */
func (h *UploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Security: API key check
	if h.APIKey != "" && r.Header.Get("X-SQRLL-API-KEY") != h.APIKey {
		writeError(w, "forbidden", http.StatusForbidden)
		return
	}

	// OOM shield: limit body size
	r.Body = http.MaxBytesReader(w, r.Body, h.MaxBytes)
	defer r.Body.Close()

	// Read entire body into memory (capped by MaxBytesReader)
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

	// MIME magic byte detection (first 512 bytes)
	mimeType := http.DetectContentType(data)
	if !config.AllowedMIMETypes[mimeType] {
		log.Printf("Rejected upload: detected MIME %s is not allowed", mimeType)
		writeError(w, "unsupported file type", http.StatusUnsupportedMediaType)
		return
	}

	// SHA-256 content hash
	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])

	// Dedup: check if file already exists on disk
	if h.Storage.Exists(hashHex) {
		h.Cache.Put(hashHex, data)
		writeJSON(w, http.StatusOK, uploadResponse{
			ID:     hashHex,
			Status: "duplicate",
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
	h.Cache.Put(hashHex, data)

	writeJSON(w, http.StatusCreated, uploadResponse{
		ID:     hashHex,
		Status: "ok",
		Size:   int64(len(data)),
	})
}

/** writeJSON serializes a struct to JSON and writes it to the response. */
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

/** writeError sends a JSON error response. */
func writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(errorResponse{Error: msg, Code: code})
}
