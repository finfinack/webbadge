// Command webbadge serves the e-ink badge preparation and Web Bluetooth UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"webbadge/pkg/server"
)

var (
	Commit  = "-"
	Version = "dev-local"
)

var (
	address  = flag.String("address", "127.0.0.1:8080", "HTTP listen address")
	logLevel = flag.String("log", "info", "log level: debug, info, warn, or error")
)

func main() {
	flag.Parse()

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))

	ctx := context.Background()

	slog.Info("starting webbadge", "version", Version, "commit", Commit)
	if err := run(ctx, *address); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf(
			"unsupported log level %q (use debug, info, warn, or error)", value)
	}
}

func run(ctx context.Context, address string) error {
	handler, err := server.New()
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	httpServer := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("webbadge listening", "url", "http://"+address)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	// The signal context is already canceled here; detach cancellation so the
	// server still gets its graceful-shutdown window.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	err = <-errCh
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen after shutdown: %w", err)
	}
	return nil
}
