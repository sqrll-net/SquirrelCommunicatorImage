package handlers

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"sync"
	"time"

	"sqrll.net/squirrel-communicator-image/internal/auth"
)

// Master-key brute-force hardening: after maxMasterFailures consecutive failed
// attempts, key management is locked out for masterLockout.
const (
	maxMasterFailures = 5
	masterLockout     = 5 * time.Minute
)

// KeyHandler manages API key registration and revocation via S2S admin endpoints.
type KeyHandler struct {
	Auth      *auth.Manager
	MasterKey string

	mu          sync.Mutex
	failCount   int
	lockedUntil time.Time
}

// ServeHTTP routes key management requests by HTTP method.
func (h *KeyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.addKey(w, r)
	case http.MethodDelete:
		h.removeKey(w, r)
	default:
		writeError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authorizeMaster verifies the admin key required for key management, with a
// lockout after repeated failures to slow brute-force guessing of the master key.
func (h *KeyHandler) authorizeMaster(w http.ResponseWriter, r *http.Request) bool {
	if h.MasterKey == "" {
		writeError(w, "key management disabled", http.StatusForbidden)
		return false
	}

	h.mu.Lock()
	if time.Now().Before(h.lockedUntil) {
		h.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(h.lockedUntil).Seconds())+1))
		writeError(w, "too many attempts", http.StatusTooManyRequests)
		return false
	}
	h.mu.Unlock()

	got := r.Header.Get("X-SQRLL-IMAGE-API-KEY")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.MasterKey)) == 1 {
		h.mu.Lock()
		h.failCount = 0
		h.mu.Unlock()
		return true
	}

	h.mu.Lock()
	h.failCount++
	if h.failCount >= maxMasterFailures {
		h.lockedUntil = time.Now().Add(masterLockout)
		h.failCount = 0
	}
	h.mu.Unlock()

	writeError(w, "forbidden", http.StatusForbidden)
	return false
}

// addKey generates and registers a new API key, returning the plaintext once.
func (h *KeyHandler) addKey(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeMaster(w, r) {
		return
	}

	key, err := h.Auth.Generate()
	if err != nil {
		writeError(w, "failed to generate key", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"key": key})
}

// removeKey revokes an API key identified by the X-SQRLL-API-KEY header.
func (h *KeyHandler) removeKey(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeMaster(w, r) {
		return
	}

	key := r.Header.Get("X-SQRLL-API-KEY")
	if key == "" {
		writeError(w, "missing key to revoke", http.StatusBadRequest)
		return
	}

	if !h.Auth.Remove(key) {
		writeError(w, "key not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
