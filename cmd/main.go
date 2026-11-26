// Command kiwix-operator is the single binary behind the manager, librarian and upload roles.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embeds the zone database, which the image does not ship

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/jacaudi/kiwix-operator/internal/libclient"
	"github.com/jacaudi/kiwix-operator/internal/librarian"
	"github.com/jacaudi/kiwix-operator/internal/manager"
	"github.com/jacaudi/kiwix-operator/internal/upload"
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

// subcommands maps each role to its entry point.
var subcommands = map[string]subcommand{
	"manager":   runManager,
	"librarian": runLibrarian,
	"upload":    runUpload,
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

// runManager runs the controller manager until SIGINT or SIGTERM; a second signal kills a stuck shutdown.
func runManager(args []string, stdout, stderr io.Writer) int {
	f, err := manager.ParseFlags(args, os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	var level slog.Level
	err = level.UnmarshalText([]byte(f.LogLevel))
	logger, lerr := newLogger(stdout, f.LogFormat, level)
	if err := errors.Join(err, lerr); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	// controller-runtime and client-go (leader election) log through these, so every line is in the chosen format.
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))
	klog.SetSlogLogger(logger)
	f.UserAgent = "kiwix-operator/" + version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore the default handling so a second one kills the process.
	context.AfterFunc(ctx, stop)
	logger.Info("starting manager", "version", version, "commit", commit, "date", date)
	if err := manager.Run(ctx, f); err != nil {
		logger.Error("manager stopped", "error", err)
		return 1
	}
	return 0
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

// tokenPath is where the crawl pod mounts the attempt's upload token; a variable so a test can point it elsewhere.
var tokenPath = "/var/run/kiwix/token" //nolint:gosec // a file path, not a credential

// Environment the crawl pod gives the upload container.
const (
	librarianURLEnv = "LIBRARIAN_URL"
	attemptEnv      = "ATTEMPT"
)

// runUpload sends the finished crawl in --output to the librarian and exits 0 once it is stored; a bad configuration exits 2.
func runUpload(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg upload.Config
	fs.StringVar(&cfg.OutputDir, "output", "/output", "directory holding the crawl's ZIM and build dir")
	fs.BoolVar(&cfg.KeepWarcs, "keep-warcs", false, "also upload a tar of the crawl's WARCs")
	fs.DurationVar(&cfg.Budget, "budget", 6*time.Hour, "total time to keep retrying before giving up")
	fs.DurationVar(&cfg.MaxBackoff, "max-backoff", 5*time.Minute, "longest wait between retries")
	logFormat := fs.String("log-format", "json", "log format: json or text")
	var logLevel slog.Level
	fs.TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var errs []error
	cfg.Attempt = os.Getenv(attemptEnv)
	if cfg.Attempt == "" {
		errs = append(errs, errors.New(attemptEnv+" is required"))
	}
	baseURL, err := librarianURL(os.Getenv(librarianURLEnv))
	errs = append(errs, err)
	cfg.Token, err = readToken(tokenPath)
	errs = append(errs, err)
	logger, err := newLogger(stdout, *logFormat, logLevel)
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if err := cfg.Validate(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	cfg.Logger = logger

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore the default handling so a second one kills the process.
	context.AfterFunc(ctx, stop)
	logger.Info("uploading crawl", "attempt", cfg.Attempt, "output", cfg.OutputDir, "keepWarcs", cfg.KeepWarcs)
	if err := upload.Run(ctx, cfg, &libclient.Client{BaseURL: baseURL}); err != nil {
		logger.Error("upload failed", "error", err)
		return 1
	}
	logger.Info("upload complete", "attempt", cfg.Attempt)
	return 0
}

// librarianURL checks raw is an http(s) URL.
func librarianURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New(librarianURLEnv + " is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s %q: want an http or https URL such as http://kiwix-librarian:8090", librarianURLEnv, raw)
	}
	return raw, nil
}

// readToken reads the upload token at path once, trimmed of whitespace.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is tokenPath, fixed by the pod spec
	if err != nil {
		return "", fmt.Errorf("read upload token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("upload token file %s is empty", path)
	}
	return token, nil
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
