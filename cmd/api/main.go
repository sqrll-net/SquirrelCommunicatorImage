package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"sqrll.net/squirrel-communicator-image/internal/auth"
	"sqrll.net/squirrel-communicator-image/internal/cache"
	"sqrll.net/squirrel-communicator-image/internal/config"
	"sqrll.net/squirrel-communicator-image/internal/handlers"
	"sqrll.net/squirrel-communicator-image/internal/singleflight"
	"sqrll.net/squirrel-communicator-image/internal/storage"
)

// corsMiddleware sets CORS headers for loopback origins (localhost/127.0.0.1/::1),
// any explicitly configured origin, or any origin when "*" is configured. Loopback
// origins are always allowed so local browser testing works out of the box;
// remote origins are only allowed when listed in SQRLL_CORS_ORIGINS (or via "*").
func corsMiddleware(allowed []string, next http.Handler) http.Handler {
	allowAll := false
	allowSet := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		if o == "*" {
			allowAll = true
		}
		allowSet[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if origin != "" && (allowAll || allowSet[origin] || isLocalhostOrigin(origin)) {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-SQRLL-API-KEY, X-SQRLL-IMAGE-API-KEY, X-API-Token")
		}

		// Handle preflight OPTIONS requests. If the Origin was not allowed
		// above, no CORS headers are present and the browser blocks the request.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isLocalhostOrigin reports whether the Origin header points at a loopback host
// (localhost, 127.0.0.1, or ::1). Loopback origins are always allowed so local
// browser testing works without opening CORS to arbitrary remote origins.
func isLocalhostOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func main() {
	cfg := config.Load()

	log.Printf("Starting SquirrelCommunicatorImage")
	log.Printf("  Storage: %s (max %d GB)", cfg.StoragePath, cfg.MaxDiskGB)
	log.Printf("  RAM cache: %d MB", cfg.MaxRAMMB)
	log.Printf("  Max upload: %d MB", cfg.MaxUploadMB)
	log.Printf("  Rate limit: %d req/hour/key", cfg.MaxRequestsPerHour)
	log.Printf("  CORS origins: %v", cfg.CORSOrigins)
	if cfg.KlipyAPIKey != "" {
		log.Printf("  GIF provider: configured")
	} else {
		log.Printf("  GIF provider: NOT configured (GIF endpoints return 503)")
	}

	// Convert config units to bytes
	maxDiskBytes := cfg.MaxDiskGB * (1 << 30) // GB to bytes
	maxRAMBytes := cfg.MaxRAMMB * (1 << 20)   // MB to bytes
	maxUploadBytes := cfg.MaxUploadMB * (1 << 20)

	// Initialize RAM cache (hot path)
	c := cache.New(maxRAMBytes)

	// Initialize storage manager (cold path + boot scan)
	store, err := storage.New(cfg.StoragePath, maxDiskBytes, c)
	if err != nil {
		log.Fatalf("Storage init failed: %v", err)
	}

	// Initialize auth manager and load bootstrap keys
	authMgr := auth.NewManager(cfg.MaxRequestsPerHour)
	for _, k := range cfg.AuthKeys {
		if err := authMgr.Add(k); err != nil {
			log.Printf("Skipping bootstrap key: %v", err)
		}
	}
	log.Printf("  Loaded %d bootstrap key(s)", authMgr.Count())

	// Handlers
	uploadHandler := &handlers.UploadHandler{
		Storage:  store,
		Cache:    c,
		Auth:     authMgr,
		MaxBytes: maxUploadBytes,
	}
	downloadHandler := &handlers.DownloadHandler{
		Storage: store,
		Cache:   c,
		Load:    new(singleflight.Group),
	}
	keyHandler := &handlers.KeyHandler{
		Auth:      authMgr,
		MasterKey: cfg.MasterKey,
	}
	gifsHandler := handlers.NewGifsHandler(authMgr, cfg.KlipyAPIKey, store, c)

	// Routes
	mux := http.NewServeMux()
	mux.Handle("/api/image/upload", uploadHandler)
	mux.Handle("/api/image/", downloadHandler)
	mux.Handle("/api/key", keyHandler)
	mux.Handle("/api/gifs/", gifsHandler)

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Bind address: SQRLL_IMAGE_SERVICE_URL (empty = all interfaces) : SQRLL_IMAGE_PORT.
	// net.JoinHostPort handles IPv6 literals (adds brackets) and empty host correctly.
	addr := net.JoinHostPort(cfg.ServiceURL, strconv.Itoa(cfg.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           corsMiddleware(cfg.CORSOrigins, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.ReadTimeoutSeconds) * time.Second,
		WriteTimeout:      time.Duration(cfg.WriteTimeoutSeconds) * time.Second,
		IdleTimeout:       time.Duration(cfg.IdleTimeoutSeconds) * time.Second,
	}

	// Graceful shutdown: drain in-flight requests before exiting
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received signal %v, draining connections", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	log.Printf("Listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server error: %v", err)
	}
}
