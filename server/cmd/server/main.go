package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"server/internal/apikey"
	"server/internal/db"
	"server/internal/db/data"
	"server/internal/httpapi"
)

const (
	defaultListenAddress = ":8080"
	shutdownTimeout      = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, logger *slog.Logger) error {
	commandName := "serve"
	if len(args) > 0 {
		commandName = args[0]
		args = args[1:]
	}
	var apiKeyCommand *apikey.Command
	switch commandName {
	case "serve":
		if len(args) != 0 {
			return errors.New("serve does not accept arguments")
		}
	case "apikey":
		var err error
		apiKeyCommand, err = apikey.Parse(args, stdout)
		if err != nil || apiKeyCommand == nil {
			return err
		}
	default:
		return fmt.Errorf("unknown command %q", commandName)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	queries := data.New(pool)
	if apiKeyCommand != nil {
		return apiKeyCommand.Execute(ctx, queries, stdout)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	address := os.Getenv("LISTEN_ADDR")
	if address == "" {
		address = defaultListenAddress
	}
	authenticator := httpapi.NewTokenAuthenticator(queries, logger)
	controlPlane := httpapi.New(pool, queries, authenticator, logger)
	return serve(ctx, address, controlPlane.Handler, logger)
}

func serve(ctx context.Context, address string, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}
	defer server.Close()
	logger.Info("server listening", "address", listener.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving http: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down http server: %w", err)
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving http: %w", err)
		}
		return nil
	}
}
