package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

/** maxKeyLen caps the plaintext length of a key to prevent abuse with huge inputs. */
const maxKeyLen = 1024

var (
	ErrKeyEmpty   = errors.New("key is empty")
	ErrKeyTooLong = errors.New("key exceeds maximum length")
	ErrKeyExists  = errors.New("key already registered")
)

/** Key holds the hash of a registered API key and its per-key rate limiting state. */
type Key struct {
	hash      [32]byte
	createdAt time.Time
	window    []time.Time // upload timestamps inside the trailing window
}

/** Manager stores registered API keys by SHA-256 hash and enforces per-key rate limits. */
type Manager struct {
	mu         sync.RWMutex
	keys       map[[32]byte]*Key
	maxPerHour int
	windowSize time.Duration
}

/** NewManager creates a Manager with the given per-key hourly request limit.
 *  A limit <= 0 disables rate limiting. */
func NewManager(maxPerHour int) *Manager {
	return &Manager{
		keys:       make(map[[32]byte]*Key),
		maxPerHour: maxPerHour,
		windowSize: time.Hour,
	}
}

/** hashKey returns the SHA-256 digest of a plaintext key. */
func hashKey(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

/** Add registers a plaintext key. Only the hash is stored, never the key itself. */
func (m *Manager) Add(plaintext string) error {
	if plaintext == "" {
		return ErrKeyEmpty
	}
	if len(plaintext) > maxKeyLen {
		return ErrKeyTooLong
	}

	h := hashKey(plaintext)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.keys[h]; ok {
		return ErrKeyExists
	}

	m.keys[h] = &Key{hash: h, createdAt: time.Now()}
	return nil
}

/** Generate creates a random 64-char hex key, registers it, and returns it once. */
func (m *Manager) Generate() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	key := hex.EncodeToString(b)
	if err := m.Add(key); err != nil {
		return "", err
	}
	return key, nil
}

/** Remove deletes a key by plaintext. Returns false if it was not registered. */
func (m *Manager) Remove(plaintext string) bool {
	h := hashKey(plaintext)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.keys[h]; !ok {
		return false
	}
	delete(m.keys, h)
	return true
}

/** Authenticate reports whether the plaintext key is registered using a
 *  constant-time comparison to avoid leaking key material via timing. */
func (m *Manager) Authenticate(plaintext string) bool {
	if plaintext == "" {
		return false
	}

	h := hashKey(plaintext)

	m.mu.RLock()
	k, ok := m.keys[h]
	m.mu.RUnlock()

	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare(h[:], k.hash[:]) == 1
}

/** Allow applies the per-key rate limit. It records the request and returns
 *  (true, 0) when allowed, or (false, retryAfter) when the limit is exceeded. */
func (m *Manager) Allow(plaintext string) (bool, time.Duration) {
	h := hashKey(plaintext)

	m.mu.Lock()
	defer m.mu.Unlock()

	k, ok := m.keys[h]
	if !ok {
		return false, 0
	}

	// Rate limiting disabled
	if m.maxPerHour <= 0 {
		return true, 0
	}

	now := time.Now()
	cutoff := now.Add(-m.windowSize)

	// Drop timestamps older than the sliding window.
	i := 0
	for i < len(k.window) && k.window[i].Before(cutoff) {
		i++
	}
	k.window = k.window[i:]

	if len(k.window) >= m.maxPerHour {
		retry := k.window[0].Add(m.windowSize).Sub(now)
		if retry < 0 {
			retry = 0
		}
		return false, retry
	}

	k.window = append(k.window, now)
	return true, 0
}

/** Count returns the number of currently registered keys. */
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.keys)
}
