// Command kiwix-operator is the single binary behind the manager, librarian and upload roles.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // embeds the zone database, which the image does not ship

	"github.com/jacaudi/kiwix-operator/internal/librarian"
)

// Stamped at build time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// adminTokenEnv names the environment variable holding the librarian's admin token.
const adminTokenEnv = "KIWIX_LIBRARIAN_ADMIN_TOKEN"

type subcommand func(args []string, stdout, stderr io.Writer) int

// subcommands maps each role to its entry point; the not-yet-wired stubs are replaced as each role lands.
var subcommands = map[string]subcommand{
	"manager":   notWired("manager"),
	"librarian": runLibrarian,
	"upload":    notWired("upload"),
}

func notWired(name string) subcommand {
	return func(_ []string, _, stderr io.Writer) int {
		_, _ = fmt.Fprintf(stderr, "%s: not yet wired\n", name)
		return 3
	}
}

func versionString() string {
	return fmt.Sprintf("kiwix-operator %s (commit %s, built %s)", version, commit, date)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, versionString())
		return 0
	}
	if len(args) > 0 {
		if sub, ok := subcommands[args[0]]; ok {
			return sub(args[1:], stdout, stderr)
		}
	}
	_, _ = fmt.Fprintln(stderr, "usage: kiwix-operator <manager|librarian|upload>")
	return 2
}

// runLibrarian runs the librarian sidecar until SIGINT or SIGTERM; a second signal kills a stuck shutdown.
func runLibrarian(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("librarian", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := librarian.Config{UserAgent: "kiwix-operator/" + version}
	fs.StringVar(&cfg.DataDir, "data-dir", "/data", "volume holding the ZIM files, library.xml and the librarian's state")
	fs.StringVar(&cfg.Listen, "listen", ":8090", "address the librarian API listens on")
	fs.StringVar(&cfg.KiwixServeURL, "kiwix-serve-url", "http://127.0.0.1:8080", "kiwix-serve, asked to confirm each library write")
	fs.Int64Var(&cfg.Reserve, "reserve-space", 5368709120, "free bytes no upload or fetch may eat into")
	fs.IntVar(&cfg.MaxFetches, "max-fetches", 1, "concurrent fetch downloads")
	fs.StringVar(&cfg.KiwixManageBin, "kiwix-manage", "/usr/local/bin/kiwix-manage", "kiwix-manage executable")
	logFormat := fs.String("log-format", "json", "log format: json or text")
	var logLevel slog.Level
	fs.TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var errs []error
	cfg.AdminToken = os.Getenv(adminTokenEnv)
	if cfg.AdminToken == "" {
		errs = append(errs, errors.New(adminTokenEnv+" is required"))
	}
	logger, err := newLogger(stdout, *logFormat, logLevel)
	if err := errors.Join(append(errs, err)...); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	cfg.Logger = logger

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore the default handling so a second one kills the process.
	context.AfterFunc(ctx, stop)
	logger.Info("starting librarian", "version", version, "commit", commit, "date", date)
	if err := librarian.Run(ctx, cfg); err != nil {
		logger.Error("librarian stopped", "error", err)
		return 1
	}
	return 0
}

// newLogger writes JSON or text logs at level to w.
func newLogger(w io.Writer, format string, level slog.Level) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("log format %q: want json or text", format)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
