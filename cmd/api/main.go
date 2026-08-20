package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/sqrll-net/squirrel-communicator-image/internal/cache"
	"github.com/sqrll-net/squirrel-communicator-image/internal/config"
	"github.com/sqrll-net/squirrel-communicator-image/internal/handlers"
	"github.com/sqrll-net/squirrel-communicator-image/internal/storage"
)

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*") // Restrict to a specific domain in production
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-SQRLL-API-KEY")

		// Handle preflight OPTIONS requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	cfg := config.Load()

	log.Printf("Starting SquirrelCommunicatorImage")
	log.Printf("  Storage: %s (max %d GB)", cfg.StoragePath, cfg.MaxDiskGB)
	log.Printf("  RAM cache: %d MB", cfg.MaxRAMMB)
	log.Printf("  Max upload: %d MB", cfg.MaxUploadMB)

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

	// Handlers
	uploadHandler := &handlers.UploadHandler{
		Storage:  store,
		Cache:    c,
		APIKey:   cfg.APIKey,
		MaxBytes: maxUploadBytes,
	}
	downloadHandler := &handlers.DownloadHandler{
		Storage: store,
		Cache:   c,
	}

	// Routes
	mux := http.NewServeMux()
	mux.Handle("/api/image/upload", uploadHandler)
	mux.Handle("/api/image/", downloadHandler)

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down", sig)
		os.Exit(0)
	}()

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("Listening on %s", addr)

	if err := http.ListenAndServe(addr, corsMiddleware(mux)); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
