package config

import (
	"os"
	"strconv"
)

/** AllowedMIMETypes for upload validation. Only these types pass the MIME check. */
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
	StoragePath string
	APIKey      string
	MaxDiskGB   int64
	MaxRAMMB    int64
	MaxUploadMB int64
	Port        int
}

/** Load reads configuration from environment variables with sensible defaults. */
func Load() *Config {
	return &Config{
		StoragePath: envStr("STORAGE_PATH", "/var/lib/sqrll/media"),
		APIKey:      envStr("SQRLL_IMAGE_API_KEY", ""),
		MaxDiskGB:   envInt64("MAX_DISK_GB", 200),
		MaxRAMMB:    envInt64("MAX_RAM_MB", 1024),
		MaxUploadMB: envInt64("MAX_UPLOAD_MB", 8),
		Port:        envInt("PORT", 8083),
	}
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
