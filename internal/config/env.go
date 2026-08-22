package config

import (
	"os"
	"strconv"
	"strings"
)

/** AllowedMIMETypes lists the MIME types accepted for upload. */
var AllowedMIMETypes = map[string]bool{
	"image/jpeg":      true,
	"image/png":       true,
	"image/gif":       true,
	"image/webp":      true,
	"image/svg+xml":   true,
	"image/bmp":       true,
	"image/tiff":      true,
	"video/mp4":       true,
	"video/webm":      true,
	"video/ogg":       true,
	"audio/mpeg":      true,
	"audio/ogg":       true,
	"audio/wav":       true,
	"audio/webm":      true,
	"application/pdf": true,
}

/** Config holds all runtime configuration loaded from environment variables. */
type Config struct {
	StoragePath        string
	MasterKey          string
	AuthKeys           []string
	KlipyAPIKey        string
	MaxRequestsPerHour int
	MaxDiskGB          int64
	MaxRAMMB           int64
	MaxUploadMB        int64
	Port               int
}

/** Load reads configuration from environment variables with sensible defaults. */
func Load() *Config {
	return &Config{
		StoragePath:        envStr("STORAGE_PATH", "/var/data/sqrll/media"),
		MasterKey:          envStr("SQRLL_IMAGE_API_KEY", ""),
		AuthKeys:           envList("SQRLL_AUTH_KEYS"),
		KlipyAPIKey:        envStr("SQRLL_KLIPY_API_KEY", ""),
		MaxRequestsPerHour: envInt("MAX_REQUESTS_PER_HOUR", 100),
		MaxDiskGB:          envInt64("MAX_DISK_GB", 100),
		MaxRAMMB:           envInt64("MAX_RAM_MB", 1024),
		MaxUploadMB:        envInt64("MAX_UPLOAD_MB", 8),
		Port:               envInt("PORT", 8083),
	}
}

/** envList reads a comma-separated list from the given variable. */
func envList(name string) []string {
	var keys []string
	if v := os.Getenv(name); v != "" {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
