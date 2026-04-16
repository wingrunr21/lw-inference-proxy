package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/wingrunr21/lw-inference-proxy/internal/config"
	"github.com/wingrunr21/lw-inference-proxy/internal/docker"
	"github.com/wingrunr21/lw-inference-proxy/internal/proxy"
	"github.com/wingrunr21/lw-inference-proxy/internal/router"
	"github.com/wingrunr21/lw-inference-proxy/internal/telemetry"
)

func main() {
	doHealthCheck := flag.Bool("healthcheck", false, "run a health check against the running proxy and exit")
	flag.Parse()

	if *doHealthCheck {
		runHealthCheck()
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := config.Load()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	otelShutdown, err := telemetry.Setup(ctx, cfg)
	if err != nil {
		slog.Error("telemetry setup failed", "error", err)
		os.Exit(1)
	}

	r := router.New()

	watcher, err := docker.NewWatcher(cfg, r)
	if err != nil {
		slog.Error("docker watcher init failed", "error", err)
		os.Exit(1)
	}
	go watcher.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("/", proxy.NewHandler(cfg, r))

	server := &http.Server{
		Addr:    cfg.Addr(),
		Handler: mux,
	}

	go func() {
		slog.Info("proxy listening", "addr", cfg.Addr())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			cancel()
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.DrainTimeout)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}
	if err := otelShutdown(shutdownCtx); err != nil {
		slog.Error("otel shutdown error", "error", err)
	}

	slog.Info("shutdown complete")
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct { //nolint:errcheck
		Status string `json:"status"`
	}{Status: "ok"})
}

// runHealthCheck is invoked when -healthcheck is passed. It performs a single
// GET /healthz against the running proxy and exits 0 on success, 1 on failure.
// Reads PROXY_PORT so it uses the same default as the server.
func runHealthCheck() {
	port := os.Getenv("PROXY_PORT")
	if port == "" {
		port = "8080"
	}
	resp, err := http.Get("http://localhost:" + port + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed: status %d\n", resp.StatusCode)
		os.Exit(1)
	}
}
