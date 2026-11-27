// Command sink is the e2e webhook receiver: it records every {"title","message"} notification POSTed to it and lists them at GET /received.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// maxBody bounds one notification body.
const maxBody = 1 << 20

func main() {
	addr := flag.String("listen", ":8080", "address to listen on")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("listening", "addr", *addr)
	if err := serve(*addr, newHandler()); err != nil {
		logger.Error("sink stopped", "error", err)
		os.Exit(1)
	}
}

type notification struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// newHandler records each POST body that is exactly a titled notification and rejects anything else with 400.
func newHandler() http.Handler {
	var mu sync.Mutex
	// Not nil, so /received lists an empty sink as [].
	got := []notification{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		var n notification
		err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxBody), &n, json.RejectUnknownMembers(true))
		if err != nil || n.Title == "" {
			http.Error(w, `want exactly {"title","message"} JSON with a title`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /received", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, got)
	})
	return mux
}

// serve runs h on addr until SIGINT or SIGTERM, then drains in-flight requests.
func serve(addr string, h http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
