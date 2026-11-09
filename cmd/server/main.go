// Command server runs the profanity filter HTTP API.
//
// Configuration (environment variables):
//
//	PORT               TCP port to listen on (default 8080)
//	WORDLIST_DIR       directory with one <lang>.txt file per language;
//	                   when unset, the lists embedded from ./wordlists are used
//	DEFAULT_LANGUAGES  comma-separated languages applied when a request does
//	                   not choose, e.g. "pt-br" or "pt-br,en" (default: every
//	                   language except opt-in "*-strict" dictionaries)
//	STATE_FILE         where reports and admin decisions are persisted
//	                   (default data/state.json)
//	ADMIN_TOKEN        bearer token for /v1/admin/*; unset disables admin routes
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jp-epaminondas/profanity-filter/internal/api"
	"github.com/jp-epaminondas/profanity-filter/internal/clock"
	"github.com/jp-epaminondas/profanity-filter/internal/moderation"
	"github.com/jp-epaminondas/profanity-filter/internal/wordlist"
	"github.com/jp-epaminondas/profanity-filter/wordlists"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Dictionaries come from the binary itself unless WORDLIST_DIR points
	// somewhere else. Both are an fs.FS, so the loading code is the same.
	var src fs.FS = wordlists.FS
	srcName := "embedded"
	if dir := os.Getenv("WORDLIST_DIR"); dir != "" {
		src, srcName = os.DirFS(dir), dir
	}
	dicts, err := wordlist.LoadDir(src)
	if err != nil {
		return err
	}

	// The first Trie is built before the server accepts any traffic, with the
	// admin decisions saved in STATE_FILE already applied.
	statePath := envOr("STATE_FILE", "data/state.json")
	sw := clock.Start()
	svc, err := moderation.New(dicts, statePath)
	if err != nil {
		return err
	}
	took := sw.Elapsed()
	f := svc.Filter()
	for _, l := range f.Languages() {
		log.Info("dictionary loaded",
			"lang", l.Code, "words", l.Words, "opt_in", l.OptIn, "source", srcName)
	}
	log.Info("trie built", "words", f.Words(), "nodes", f.Nodes(), "took", took.String())
	log.Info("moderation state", "file", statePath, "decisions", len(svc.Words()))

	// DEFAULT_LANGUAGES pre-selects the dictionaries applied when a request
	// does not send "languages". Unset means every non opt-in language.
	defaults := f.Defaults()
	if v := os.Getenv("DEFAULT_LANGUAGES"); v != "" {
		if defaults, err = f.Resolve(strings.Split(v, ",")); err != nil {
			return fmt.Errorf("DEFAULT_LANGUAGES: %w", err)
		}
	}
	log.Info("default languages", "langs", strings.Join(f.Codes(defaults), ","))

	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		log.Warn("ADMIN_TOKEN not set: /v1/admin routes are disabled")
	}

	srv := &http.Server{
		Addr: ":" + envOr("PORT", "8080"),
		Handler: api.NewRouter(svc, api.Config{
			Defaults:   defaults,
			AdminToken: adminToken,
		}, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Reports are saved in the background every few seconds instead of on
	// every request, so a burst of complaints does not turn into a burst of
	// disk writes. Admin decisions are saved immediately.
	go flushLoop(ctx, svc, log)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting new connections and let in-flight
	// requests finish.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if err := svc.Flush(); err != nil {
		return fmt.Errorf("save moderation state: %w", err)
	}
	return shutdownErr
}

func flushLoop(ctx context.Context, svc *moderation.Service, log *slog.Logger) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := svc.Flush(); err != nil {
				log.Error("save moderation state", "err", err)
			}
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
