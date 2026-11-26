// Package manager runs the operator's controller manager: its flags, scheme, cache and both reconcilers.
package manager

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/catalog"
	"github.com/jacaudi/kiwix-operator/internal/controller"
	"github.com/jacaudi/kiwix-operator/internal/libclient"
	"github.com/jacaudi/kiwix-operator/internal/notify"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

const (
	leaderElectionID = "kiwix-operator.kiwix.jacaudi.dev"
	eventSource      = "kiwix-operator"
	gatewayAPIGroup  = "gateway.networking.k8s.io/v1"
	defaultCatalog   = "https://opds.library.kiwix.org"
)

// Flags configure the manager; ParseFlags fills and validates every field except UserAgent.
type Flags struct {
	MetricsAddr, ProbeAddr, CatalogURL, OperatorImage, ZimitImage, OperatorNamespace, LogFormat, LogLevel string
	WebhookRetries                                                                                        int
	EgressAllowCIDRs, ProbeAllowCIDRs                                                                     []string
	LeaderElect                                                                                           bool
	// UserAgent is sent on outbound catalog and webhook requests; the caller sets it, it is not a flag.
	UserAgent string
}

// ParseFlags parses the manager's flags, falling back to getenv for those with an environment variable, and reports every problem at once.
func ParseFlags(args []string, getenv func(string) string) (Flags, error) {
	var f Flags
	var errs []error
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.MetricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	fs.StringVar(&f.ProbeAddr, "health-probe-bind-address", ":8081", "address the health probes bind to")
	fs.StringVar(&f.CatalogURL, "catalog-url", cmp.Or(getenv("CATALOG_URL"), defaultCatalog), "Kiwix OPDS catalog root (CATALOG_URL)")
	fs.StringVar(&f.OperatorImage, "operator-image", getenv("OPERATOR_IMAGE"), "image running the librarian and the crawl upload (OPERATOR_IMAGE)")
	fs.StringVar(&f.ZimitImage, "zimit-image", getenv("ZIMIT_IMAGE"), "image running crawls (ZIMIT_IMAGE)")
	fs.StringVar(&f.OperatorNamespace, "operator-namespace", getenv("POD_NAMESPACE"), "namespace the manager runs in (POD_NAMESPACE)")
	retries := 3
	if v := getenv("WEBHOOK_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("WEBHOOK_RETRIES %q: want an integer", v))
		} else {
			retries = n
		}
	}
	fs.IntVar(&f.WebhookRetries, "webhook-retries", retries, "times a failed webhook notification is retried (WEBHOOK_RETRIES)")
	egress := fs.String("crawl-egress-allow-cidrs", getenv("CRAWL_EGRESS_ALLOW_CIDRS"), "comma-separated CIDRs added to kiwix and crawl pod egress (CRAWL_EGRESS_ALLOW_CIDRS)")
	probe := fs.String("probe-allow-cidrs", getenv("PROBE_ALLOW_CIDRS"), "comma-separated CIDRs allowed to reach the kiwix pod ports (PROBE_ALLOW_CIDRS)")
	fs.BoolVar(&f.LeaderElect, "leader-elect", false, "elect one active manager among replicas")
	fs.StringVar(&f.LogFormat, "log-format", "json", "log format: json or text")
	fs.StringVar(&f.LogLevel, "log-level", "info", "log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return Flags{}, err
	}

	for _, req := range []struct{ flag, env, value string }{
		{"--operator-image", "OPERATOR_IMAGE", f.OperatorImage},
		{"--zimit-image", "ZIMIT_IMAGE", f.ZimitImage},
		{"--operator-namespace", "POD_NAMESPACE", f.OperatorNamespace},
	} {
		if req.value == "" {
			errs = append(errs, fmt.Errorf("%s (or %s) is required", req.flag, req.env))
		}
	}
	if u, err := url.Parse(f.CatalogURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("--catalog-url %q: want an http or https URL", f.CatalogURL))
	}
	if f.WebhookRetries < 0 {
		errs = append(errs, fmt.Errorf("--webhook-retries %d: want 0 or more", f.WebhookRetries))
	}
	var err error
	f.EgressAllowCIDRs, err = parseCIDRs("--crawl-egress-allow-cidrs", *egress)
	errs = append(errs, err)
	f.ProbeAllowCIDRs, err = parseCIDRs("--probe-allow-cidrs", *probe)
	errs = append(errs, err)
	if !slices.Contains([]string{"json", "text"}, f.LogFormat) {
		errs = append(errs, fmt.Errorf("log format %q: want json or text", f.LogFormat))
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(f.LogLevel)); err != nil {
		errs = append(errs, fmt.Errorf("log level %q: want debug, info, warn or error", f.LogLevel))
	}
	if err := errors.Join(errs...); err != nil {
		return Flags{}, err
	}
	return f, nil
}

// parseCIDRs splits a comma-separated list, skipping empty entries; each entry must be a CIDR with no host bits set.
func parseCIDRs(name, list string) ([]string, error) {
	var out []string
	var errs []error
	for s := range strings.SplitSeq(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %q is not a CIDR", name, s))
		case p != p.Masked():
			errs = append(errs, fmt.Errorf("%s: %q has host bits set; want %s", name, s, p.Masked()))
		default:
			out = append(out, s)
		}
	}
	return out, errors.Join(errs...)
}

// Run starts the manager with both reconcilers and blocks until ctx is done or the manager fails.
func Run(ctx context.Context, f Flags) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(scheme), v1alpha1.AddToScheme(scheme), gwapiv1.Install(scheme)); err != nil {
		return fmt.Errorf("build scheme: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create discovery client: %w", err)
	}
	gatewayAPI, err := gatewayAPIAvailable(ctx, dc)
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: f.MetricsAddr},
		HealthProbeBindAddress:  f.ProbeAddr,
		LeaderElection:          f.LeaderElect,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: f.OperatorNamespace,
		Cache:                   cacheOptions(),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	kl, za := newReconcilers(f, mgr.GetClient(), mgr.GetScheme(), mgr.GetEventRecorder(eventSource), gatewayAPI)
	if err := kl.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up KiwixLibrary controller: %w", err)
	}
	if err := za.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up ZimArchive controller: %w", err)
	}
	if err := errors.Join(mgr.AddHealthzCheck("healthz", healthz.Ping), mgr.AddReadyzCheck("readyz", healthz.Ping)); err != nil {
		return fmt.Errorf("add health checks: %w", err)
	}
	ctrl.LoggerFrom(ctx).WithName("setup").Info("starting manager", "gatewayAPI", gatewayAPI)
	return mgr.Start(ctx)
}

// cacheOptions keep Pods, Secrets and ConfigMaps the operator did not create out of the cache; every one it creates or reads carries the managed-by label.
func cacheOptions() cache.Options {
	managed := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}:       managed,
		&corev1.Secret{}:    managed,
		&corev1.ConfigMap{}: managed,
	}}
}

// gatewayAPIAvailable reports whether the cluster serves HTTPRoutes at gateway.networking.k8s.io/v1; a discovery failure other than an absent group fails startup.
func gatewayAPIAvailable(ctx context.Context, d discovery.ServerResourcesInterfaceWithContext) (bool, error) {
	list, err := d.ServerResourcesForGroupVersionWithContext(ctx, gatewayAPIGroup)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("discover %s: %w", gatewayAPIGroup, err)
	}
	return slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == "httproutes" }), nil
}

// newReconcilers builds both reconcilers; the catalog and librarian clients share one HTTP client, each call bounded by its own timeout.
func newReconcilers(f Flags, c client.Client, scheme *runtime.Scheme, rec events.EventRecorder, gatewayAPI bool) (*controller.KiwixLibraryReconciler, *controller.ZimArchiveReconciler) {
	shared := &http.Client{}
	// A followed 301/302/303 turns the webhook POST into a body-less GET that would report success.
	webhook := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	kl := &controller.KiwixLibraryReconciler{
		Client:              c,
		Scheme:              scheme,
		Recorder:            rec,
		OperatorImage:       f.OperatorImage,
		OperatorNamespace:   f.OperatorNamespace,
		EgressAllowCIDRs:    f.EgressAllowCIDRs,
		ProbeAllowCIDRs:     f.ProbeAllowCIDRs,
		GatewayAPIAvailable: gatewayAPI,
		NewLibClient: func(baseURL, token string) controller.LibraryStatusAPI {
			return &libclient.Client{BaseURL: baseURL, Token: token, HTTP: shared}
		},
	}
	za := &controller.ZimArchiveReconciler{
		Client:        c,
		Scheme:        scheme,
		Recorder:      rec,
		OperatorImage: f.OperatorImage,
		ZimitImage:    f.ZimitImage,
		Catalog:       &catalog.Client{BaseURL: f.CatalogURL, HTTP: shared, UserAgent: f.UserAgent},
		Notifier:      &notify.Notifier{HTTP: webhook, Retries: f.WebhookRetries, UserAgent: f.UserAgent},
		NewLibrarian: func(lib *v1alpha1.KiwixLibrary, token string) controller.ArchiveLibrarian {
			return &libclient.Client{BaseURL: controller.LibrarianURL(lib), Token: token, HTTP: shared}
		},
	}
	return kl, za
}
