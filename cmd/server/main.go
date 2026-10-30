package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"opendtu-hems-telemetry/internal/auth"
	"opendtu-hems-telemetry/internal/database"
	httpserver "opendtu-hems-telemetry/internal/http"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8080/health")
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 && !(len(os.Args) == 2 && os.Args[1] == "rebuild-dashboard") {
		log.Fatal("usage: collector [healthcheck|rebuild-dashboard]")
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	username, password := os.Getenv("ADMIN_USERNAME"), os.Getenv("ADMIN_PASSWORD")
	if username == "" || password == "" {
		return errors.New("ADMIN_USERNAME and ADMIN_PASSWORD are required")
	}
	if strings.Contains(username, ":") {
		return errors.New("ADMIN_USERNAME must not contain a colon")
	}
	cipher, err := auth.NewCipher(os.Getenv("TOKEN_ENCRYPTION_KEY"))
	if err != nil {
		return err
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	store, err := database.Open(filepath.Join(dataDir, "telemetry.db"), cipher)
	if err != nil {
		return err
	}
	defer store.Close()
	if len(os.Args) == 2 && os.Args[1] == "rebuild-dashboard" {
		return store.RebuildDashboard(context.Background())
	}
	handler, err := httpserver.New(store, username, password, log.Default())
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: ":8080", Handler: handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Print("collector listening on :8080")
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			server.Close()
			return err
		}
	}
	return nil
}
