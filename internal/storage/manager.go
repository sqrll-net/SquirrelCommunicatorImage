package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"sqrll.net/squirrel-communicator-image/internal/cache"
)

// ErrDiskQuotaExceeded is returned when a write would exceed the configured disk quota.
var ErrDiskQuotaExceeded = errors.New("disk quota exceeded")

// Manager handles disk-level file operations: boot scan, writes, reads, quota.
type Manager struct {
	basePath  string
	maxSize   int64             // max total bytes allowed on disk
	totalSize atomic.Int64      // current disk usage in bytes
	extMap    map[string]string // hash -> file extension (e.g. ".png")
	cache     *cache.Cache
	mu        sync.RWMutex // protects extMap
}

// New creates a Storage Manager, scans the base path on startup to rebuild state.
func New(basePath string, maxSize int64, c *cache.Cache) (*Manager, error) {
	m := &Manager{
		basePath: basePath,
		maxSize:  maxSize,
		extMap:   make(map[string]string),
		cache:    c,
	}

	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}

	if err := m.bootScan(); err != nil {
		return nil, fmt.Errorf("boot scan: %w", err)
	}

	log.Printf("Storage boot complete: %d files, %.2f GB used", len(m.extMap), float64(m.totalSize.Load())/(1<<30))
	return m, nil
}

// bootScan walks the storage directory to rebuild the extension map and total
// size, and deletes any orphaned temp files left by an unclean shutdown.
func (m *Manager) bootScan() error {
	return filepath.WalkDir(m.basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		filename := filepath.Base(path)

		// Clean up incomplete temp files (hash.ext.tmp.suffix) left by a crash
		// between write and rename. They are never referenced by extMap.
		if strings.Contains(filename, ".tmp.") {
			if err := os.Remove(path); err != nil {
				log.Printf("cleanup orphaned temp file %s: %v", filename, err)
			} else {
				log.Printf("removed orphaned temp file %s", filename)
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		ext := filepath.Ext(filename)
		hash := filename[:len(filename)-len(ext)]

		// Basic validation: SHA-256 hex is 64 chars, must have an extension
		if len(hash) == 64 && ext != "" {
			m.extMap[hash] = ext
			m.totalSize.Add(info.Size())
		}

		return nil
	})
}

// Exists checks whether a file with the given hash already exists on disk.
func (m *Manager) Exists(hash string) bool {
	m.mu.RLock()
	ext, ok := m.extMap[hash]
	m.mu.RUnlock()

	if !ok {
		return false
	}
	_, err := os.Stat(filepath.Join(m.basePath, hash+ext))
	return err == nil
}

// Write persists file bytes to disk. The quota check and extension-map updates
// are synchronized, but the slow temp-write + rename happens outside the lock
// so different files can be written concurrently. Returns nil if the file
// already exists (dedup).
func (m *Manager) Write(hash string, data []byte, mimeType string) error {
	ext := mimeTypeToExt(mimeType)
	size := int64(len(data))

	// Fast-path dedup without blocking concurrent writes to other files.
	m.mu.RLock()
	_, exists := m.extMap[hash]
	m.mu.RUnlock()
	if exists {
		return nil
	}

	// Reserve quota atomically. Released on any failure path below via defer.
	if !m.reserveQuota(size) {
		return ErrDiskQuotaExceeded
	}
	committed := false
	defer func() {
		if !committed {
			m.totalSize.Add(-size)
		}
	}()

	filePath := filepath.Join(m.basePath, hash+ext)
	suffix, err := randomSuffix(8)
	if err != nil {
		return fmt.Errorf("generate temp name: %w", err)
	}
	tmpPath := filePath + ".tmp." + suffix

	// Write to temp file first (I/O outside the lock).
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	// Atomic rename into place.
	if err := os.Rename(tmpPath, filePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}

	// Commit bookkeeping. Re-check under lock in case a concurrent writer
	// committed the same hash first; content is identical so the overwrite is
	// harmless, but the quota must not be double-counted.
	m.mu.Lock()
	if _, ok := m.extMap[hash]; ok {
		m.mu.Unlock()
		return nil // our reservation is released by the deferred cleanup
	}
	m.extMap[hash] = ext
	m.mu.Unlock()

	committed = true
	log.Printf("Written: %s (%d bytes, total: %.2f GB)", hash+ext, size, float64(m.totalSize.Load())/(1<<30))
	return nil
}

// reserveQuota atomically reserves size bytes against the disk quota, returning
// false if the write would exceed maxSize.
func (m *Manager) reserveQuota(size int64) bool {
	for {
		cur := m.totalSize.Load()
		if cur+size > m.maxSize {
			return false
		}
		if m.totalSize.CompareAndSwap(cur, cur+size) {
			return true
		}
	}
}

// Read loads a file from disk by hash. MIME type is derived by the caller.
func (m *Manager) Read(hash string) ([]byte, error) {
	m.mu.RLock()
	ext, ok := m.extMap[hash]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("file not found: %s", hash)
	}

	data, err := os.ReadFile(filepath.Join(m.basePath, hash+ext))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return data, nil
}

// TotalSize returns the current disk usage in bytes.
func (m *Manager) TotalSize() int64 {
	return m.totalSize.Load()
}

// mimeTypeToExt returns the file extension for a MIME type (including the leading dot).
func mimeTypeToExt(mimeType string) string {
	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil || len(exts) == 0 {
		return ".bin"
	}
	// Pick the first extension from a preferred list to ensure consistency
	for _, e := range exts {
		switch e {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".bmp", ".tiff",
			".mp4", ".webm", ".ogg", ".ogv", ".mp3", ".wav", ".pdf":
			return e
		}
	}
	return exts[0]
}

// RemoveFile deletes a file from disk and the extension map (for administrative use).
func (m *Manager) RemoveFile(hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ext, ok := m.extMap[hash]
	if !ok {
		return fmt.Errorf("file not found: %s", hash)
	}

	filePath := filepath.Join(m.basePath, hash+ext)
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	if err := os.Remove(filePath); err != nil {
		return err
	}

	delete(m.extMap, hash)
	m.totalSize.Add(-info.Size())
	m.cache.Remove(hash)

	return nil
}

// randomSuffix generates a short random hex string for temp file names.
func randomSuffix(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
