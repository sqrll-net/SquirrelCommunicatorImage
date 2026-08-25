package handlers

import (
	"crypto/rand"
	"math/big"
	"net/http"
)

// base62Alphabet is the character set used for instance IDs: 0-9, A-Z, a-z.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// instanceIDLength is the fixed length of a generated instance ID (32 chars).
const instanceIDLength = 32

// NewInstanceID returns a cryptographically random 32-character Base62 string
// that uniquely identifies a running instance of this service. The value is
// generated in memory at startup and never persisted, so every process start
// yields a fresh identifier.
func NewInstanceID() (string, error) {
	out := make([]byte, instanceIDLength)
	max := big.NewInt(int64(len(base62Alphabet)))
	for i := range out {
		// crypto/rand.Int returns a uniform value in [0, max), avoiding the
		// modulo bias that a byte%62 scheme would introduce.
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = base62Alphabet[idx.Int64()]
	}
	return string(out), nil
}

// InstanceHandler serves the running instance's identifier at GET /instance.
// It is unauthenticated (like /health): the ID is a non-secret operational
// identifier, not authentication material.
type InstanceHandler struct {
	ID string
}

// ServeHTTP implements http.Handler.
func (h *InstanceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"instanceId": h.ID})
}
