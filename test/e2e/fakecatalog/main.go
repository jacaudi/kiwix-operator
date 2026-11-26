// Command fakecatalog is the e2e stand-in for the Kiwix OPDS catalog: every entries query returns one book, the -file ZIM, with its .meta4.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// feed is a one-entry OPDS acquisition feed, shaped like internal/catalog/testdata/entries-stardew.xml; the e2e test expects the 2026-10 version.
const feed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <name>%s</name>
    <flavour></flavour>
    <updated>2026-10-01T00:00:00Z</updated>
    <link rel="http://opds-spec.org/acquisition/open-access" type="application/x-zim" href="%s.meta4" length="%d" />
  </entry>
</feed>
`

const meta4 = `<?xml version="1.0" encoding="UTF-8"?>
<metalink xmlns="urn:ietf:params:xml:ns:metalink">
  <file name="%s">
    <size>%d</size>
    <hash type="sha-256">%s</hash>
    <url>%s</url>
  </file>
</metalink>
`

func main() {
	addr := flag.String("listen", ":8080", "address to listen on")
	file := flag.String("file", "/srv/book-01.zim", "the ZIM file every catalog query returns")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	h, err := newHandler(*file)
	if err == nil {
		logger.Info("listening", "addr", *addr, "file", *file)
		err = serve(*addr, h)
	}
	if err != nil {
		logger.Error("fakecatalog stopped", "error", err)
		os.Exit(1)
	}
}

// newHandler serves the catalog feed, the .meta4 and the file itself; links use the request's host, so they resolve wherever the catalog is reached.
func newHandler(file string) (http.Handler, error) {
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return nil, fmt.Errorf("read the catalog book: %w", err)
	}
	sum := sha256.Sum256(data)
	name := filepath.Base(file)
	fileURL := func(r *http.Request) string { return html.EscapeString("http://" + r.Host + "/files/" + name) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /catalog/v2/entries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml;profile=opds-catalog;kind=acquisition")
		_, _ = fmt.Fprintf(w, feed, html.EscapeString(r.URL.Query().Get("name")), fileURL(r), len(data))
	})
	mux.HandleFunc("GET /files/"+name+".meta4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/metalink4+xml")
		_, _ = fmt.Fprintf(w, meta4, html.EscapeString(name), len(data), hex.EncodeToString(sum[:]), fileURL(r))
	})
	mux.HandleFunc("GET /files/"+name, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
	return mux, nil
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
