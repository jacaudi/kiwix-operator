package v1alpha1

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const testNamespace = "default"

var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run starts envtest once for the package so every test shares one API server.
func run(m *testing.M) int {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest (is KUBEBUILDER_ASSETS set? run `task test`): %v\n", err)
		return 1
	}

	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "add scheme: %v\n", err)
		return 1
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		return 1
	}
	return m.Run()
}

func newLibrary(name string, mutate func(*KiwixLibrary)) *KiwixLibrary {
	l := &KiwixLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       KiwixLibrarySpec{Storage: StorageSpec{ExistingClaim: "zims"}},
	}
	if mutate != nil {
		mutate(l)
	}
	return l
}

func newArchive(name string, mutate func(*ZimArchive)) *ZimArchive {
	a := &ZimArchive{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: ZimArchiveSpec{
			LibraryRef: corev1.LocalObjectReference{Name: "lib"},
			Source:     SourceSpec{Crawl: &CrawlSource{Seeds: []string{"https://example.org"}}},
			// A zero ratio is valid, so the typed client sends the defaults explicitly.
			Refresh: RefreshSpec{MinArticleRatio: 80, MinSizeRatio: 80},
		},
	}
	if mutate != nil {
		mutate(a)
	}
	return a
}

func requireInvalid(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "want an Invalid error, got %v", err)
	assert.Contains(t, err.Error(), want)
}

func TestStorageExactlyOne(t *testing.T) {
	size := resource.MustParse("10Gi")
	tests := []struct {
		name    string
		storage StorageSpec
		wantErr bool
	}{
		{"both", StorageSpec{ExistingClaim: "zims", Size: &size}, true},
		{"neither", StorageSpec{}, true},
		{"claim-only", StorageSpec{ExistingClaim: "zims"}, false},
		{"size-only", StorageSpec{Size: &size}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLibrary("storage-"+tt.name, func(l *KiwixLibrary) { l.Spec.Storage = tt.storage })
			err := k8sClient.Create(t.Context(), l)
			if tt.wantErr {
				requireInvalid(t, err, "exactly one")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestStorageImmutable(t *testing.T) {
	l := newLibrary("storage-immutable", nil)
	require.NoError(t, k8sClient.Create(t.Context(), l))

	l.Spec.Storage.ExistingClaim = "other"
	requireInvalid(t, k8sClient.Update(t.Context(), l), "immutable")
}

func TestSourceExactlyOne(t *testing.T) {
	crawl := &CrawlSource{Seeds: []string{"https://example.org"}}
	download := &DownloadSource{Catalog: &CatalogRef{Name: "wikipedia_en_all", Flavour: "maxi"}}
	tests := []struct {
		name    string
		source  SourceSpec
		wantErr bool
	}{
		{"both", SourceSpec{Crawl: crawl, Download: download}, true},
		{"neither", SourceSpec{}, true},
		{"crawl-only", SourceSpec{Crawl: crawl}, false},
		{"download-only", SourceSpec{Download: download}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newArchive("source-"+tt.name, func(a *ZimArchive) { a.Spec.Source = tt.source })
			err := k8sClient.Create(t.Context(), a)
			if tt.wantErr {
				requireInvalid(t, err, "exactly one")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDownloadExactlyOne(t *testing.T) {
	tests := []struct {
		name     string
		download DownloadSource
		wantErr  bool
	}{
		{"both", DownloadSource{Catalog: &CatalogRef{Name: "x"}, URL: "https://example.org/x.zim"}, true},
		{"neither", DownloadSource{}, true},
		{"catalog-only", DownloadSource{Catalog: &CatalogRef{Name: "x"}}, false},
		{"url-only", DownloadSource{URL: "https://example.org/x.zim"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newArchive("download-"+tt.name, func(a *ZimArchive) { a.Spec.Source = SourceSpec{Download: &tt.download} })
			err := k8sClient.Create(t.Context(), a)
			if tt.wantErr {
				requireInvalid(t, err, "exactly one")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSHA256OnlyWithURL(t *testing.T) {
	sum := strings.Repeat("a", 64)
	tests := []struct {
		name     string
		download DownloadSource
		wantErr  bool
	}{
		{"catalog-with-sha", DownloadSource{Catalog: &CatalogRef{Name: "x"}, SHA256: sum}, true},
		{"url-with-sha", DownloadSource{URL: "https://example.org/x.zim", SHA256: sum}, false},
		{"bad-sha", DownloadSource{URL: "https://example.org/x.zim", SHA256: "XYZ"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newArchive("sha-"+tt.name, func(a *ZimArchive) { a.Spec.Source = SourceSpec{Download: &tt.download} })
			err := k8sClient.Create(t.Context(), a)
			if tt.wantErr {
				requireInvalid(t, err, "")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestLibraryRefImmutable(t *testing.T) {
	a := newArchive("libref-immutable", nil)
	require.NoError(t, k8sClient.Create(t.Context(), a))

	a.Spec.LibraryRef.Name = "other"
	requireInvalid(t, k8sClient.Update(t.Context(), a), "immutable")
}

func TestArchiveNameLength(t *testing.T) {
	tests := []struct {
		name    string
		length  int
		wantErr bool
	}{
		{"too-long", 46, true},
		{"at-limit", 45, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newArchive(strings.Repeat("a", tt.length), nil)
			err := k8sClient.Create(t.Context(), a)
			if tt.wantErr {
				requireInvalid(t, err, "45")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestArchiveNameDNSLabel(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
		wantMsg string
	}{
		// "bg3.wiki" is a valid object name, so only the CRD rule can reject it.
		{"bg3.wiki", true, "DNS label"},
		{"a_b", true, ""},
		{"bg3-wiki", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := k8sClient.Create(t.Context(), newArchive(tt.name, nil))
			if tt.wantErr {
				requireInvalid(t, err, tt.wantMsg)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDefaultsApplied(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GroupVersion.String(),
		"kind":       "ZimArchive",
		"metadata":   map[string]any{"name": "defaults", "namespace": testNamespace},
		"spec": map[string]any{
			"libraryRef": map[string]any{"name": "lib"},
			"source":     map[string]any{"crawl": map[string]any{"seeds": []any{"https://example.org"}}},
		},
	}}
	require.NoError(t, k8sClient.Create(t.Context(), u))

	var got ZimArchive
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKeyFromObject(u), &got))

	crawl := got.Spec.Source.Crawl
	require.NotNil(t, crawl)
	assert.Equal(t, int32(4), crawl.Workers)
	assert.Equal(t, 48*time.Hour, crawl.Timeout.Duration)
	assert.Zero(t, crawl.Scratch.Size.Cmp(resource.MustParse("20Gi")), "scratch size %s", crawl.Scratch.Size.String())
	assert.Equal(t, int32(80), got.Spec.Refresh.MinArticleRatio)
	assert.Equal(t, int32(80), got.Spec.Refresh.MinSizeRatio)
	assert.Equal(t, DeletionPolicyRetain, got.Spec.DeletionPolicy)
}

func TestLibraryDefaultsApplied(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GroupVersion.String(),
		"kind":       "KiwixLibrary",
		"metadata":   map[string]any{"name": "lib-defaults", "namespace": testNamespace},
		"spec":       map[string]any{"storage": map[string]any{"existingClaim": "zims"}},
	}}
	require.NoError(t, k8sClient.Create(t.Context(), u))

	var got KiwixLibrary
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKeyFromObject(u), &got))

	assert.Equal(t, int32(1), got.Spec.Downloads.MaxConcurrent)
	assert.Equal(t, int32(1), got.Spec.Crawls.MaxConcurrent)
	assert.Zero(t, got.Spec.ReserveSpace.Cmp(resource.MustParse("5Gi")), "reserve space %s", got.Spec.ReserveSpace.String())
}

func TestRatiosBounded(t *testing.T) {
	tests := []struct {
		name    string
		ratio   int32
		wantErr bool
	}{
		{"zero", 0, false},
		{"hundred", 100, false},
		{"over", 101, true},
		{"negative", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newArchive("ratio-"+tt.name, func(a *ZimArchive) {
				a.Spec.Refresh = RefreshSpec{MinArticleRatio: tt.ratio, MinSizeRatio: tt.ratio}
			})
			err := k8sClient.Create(t.Context(), a)
			if tt.wantErr {
				requireInvalid(t, err, "")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDeletionPolicyEnum(t *testing.T) {
	tests := []struct {
		name    string
		policy  DeletionPolicy
		wantErr bool
	}{
		{"delete", DeletionPolicyDelete, false},
		{"retain", DeletionPolicyRetain, false},
		{"unknown", DeletionPolicy("Purge"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newArchive("policy-"+tt.name, func(a *ZimArchive) { a.Spec.DeletionPolicy = tt.policy })
			err := k8sClient.Create(t.Context(), a)
			if tt.wantErr {
				requireInvalid(t, err, "deletionPolicy")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSpecRequired(t *testing.T) {
	tests := []struct {
		kind string
		name string
	}{
		{"KiwixLibrary", "spec-required-lib"},
		{"ZimArchive", "spec-required-zim"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": GroupVersion.String(),
				"kind":       tt.kind,
				"metadata":   map[string]any{"name": tt.name, "namespace": testNamespace},
			}}
			requireInvalid(t, k8sClient.Create(t.Context(), u), "spec")
		})
	}
}

func TestTimeoutFormat(t *testing.T) {
	tests := []struct {
		name    string
		timeout string
		wantErr bool
	}{
		{"hours", "48h", false},
		{"compound", "1h30m", false},
		{"days", "2d", true},
		{"bare-number", "30", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": GroupVersion.String(),
				"kind":       "ZimArchive",
				"metadata":   map[string]any{"name": "timeout-" + tt.name, "namespace": testNamespace},
				"spec": map[string]any{
					"libraryRef": map[string]any{"name": "lib"},
					"source": map[string]any{"crawl": map[string]any{
						"seeds":   []any{"https://example.org"},
						"timeout": tt.timeout,
					}},
				},
			}}
			err := k8sClient.Create(t.Context(), u)
			if tt.wantErr {
				requireInvalid(t, err, "timeout")
				return
			}
			require.NoError(t, err)
		})
	}
}
