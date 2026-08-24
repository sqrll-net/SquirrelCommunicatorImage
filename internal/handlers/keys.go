package handlers

import (
	"crypto/subtle"
	"net/http"

	"sqrll.net/squirrel-communicator-image/internal/auth"
)

/** KeyHandler manages API key registration and revocation via S2S admin endpoints. */
type KeyHandler struct {
	Auth      *auth.Manager
	MasterKey string
}

/** ServeHTTP routes key management requests by HTTP method. */
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

/** authorizeMaster verifies the admin key required for key management. */
func (h *KeyHandler) authorizeMaster(w http.ResponseWriter, r *http.Request) bool {
	if h.MasterKey == "" {
		writeError(w, "key management disabled", http.StatusForbidden)
		return false
	}

	got := r.Header.Get("X-SQRLL-IMAGE-API-KEY")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.MasterKey)) != 1 {
		writeError(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

/** addKey generates and registers a new API key, returning the plaintext once. */
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

/** removeKey revokes an API key identified by the X-SQRLL-API-KEY header. */
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
