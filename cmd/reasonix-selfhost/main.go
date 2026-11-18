// Command reasonix-selfhost is one process that serves the accounts endpoints a
// Reasonix client already calls, the approval page a person opens once, and the
// relay that carries a controller's ciphertext to a Studio.
//
// It is deliberately one binary and one SQLite file: a personal relay serves one
// person, and a deployment that needs a database server, a queue and a cache to
// let a phone reach a desktop is worse than the LAN the app already speaks.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/f1shsmell/Reasonix-SelfHost/internal/api"
	"github.com/f1shsmell/Reasonix-SelfHost/internal/config"
	"github.com/f1shsmell/Reasonix-SelfHost/internal/store"
)

// pruneEvery bounds how long an expired grant or ticket can sit in the file.
// They are refused by the queries anyway, so this only keeps the database from
// growing one row per abandoned sign-in.
const pruneEvery = 15 * time.Minute

func main() {
	if err := run(); err != nil {
		log.Fatalf("reasonix-selfhost: %v", err)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.FilePath()), 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	st, err := store.Open(cfg.FilePath())
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer st.Close()

	server, err := api.New(cfg, st)
	if err != nil {
		return err
	}
	defer server.Close()

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A device socket is a WebSocket, so the server cannot cap the whole
		// request by a deadline; each individual write is bounded instead.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go pruneLoop(ctx, st)

	errs := make(chan error, 1)
	go func() {
		log.Printf("listening on %s, accounts at %s, relay at %s", cfg.Addr, cfg.PublicURL, cfg.RelayURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdown)
}

// pruneLoop drops rows nothing can use. A failure is logged and retried on the
// next tick: a housekeeping step must never take the service down.
func pruneLoop(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.Prune(); err != nil {
				log.Printf("prune: %v", err)
			}
		}
	}
}
