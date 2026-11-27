package controller

import (
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

const (
	testZimitImage = "ghcr.io/openzim/zimit:test"
	testAttempt    = "abcdefgh"
)

func testCrawlArchive() *v1alpha1.ZimArchive {
	return &v1alpha1.ZimArchive{
		ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "kiwix", UID: "uid-site", Generation: 2},
		Spec: v1alpha1.ZimArchiveSpec{
			LibraryRef: corev1.LocalObjectReference{Name: "wiki"},
			Source: v1alpha1.SourceSpec{Crawl: &v1alpha1.CrawlSource{
				Seeds:   []string{"https://a/", "https://b/"},
				Workers: 4,
				Timeout: metav1.Duration{Duration: 48 * time.Hour},
				Scratch: v1alpha1.ScratchSpec{Size: resource.MustParse("20Gi")},
			}},
		},
	}
}

func buildTestJob(za *v1alpha1.ZimArchive) (zimit, upload corev1.Container, spec corev1.PodSpec) {
	job, _ := buildCrawlJob(za, testLib(), testAttempt, testZimitImage, testImage)
	spec = job.Spec.Template.Spec
	return spec.InitContainers[0], spec.Containers[0], spec
}

func TestCrawlJobArgs(t *testing.T) {
	base := []string{
		"--seeds", "https://a/,https://b/", "--name", "site", "--output", "/output", "--build", "/output/build",
		"--workers", "4", "--timeSoftLimit", "172800",
	}
	tests := []struct {
		name   string
		mutate func(*v1alpha1.CrawlSource)
		want   []string
	}{
		{name: "defaults", mutate: func(*v1alpha1.CrawlSource) {}, want: base},
		{
			name: "optional flags",
			mutate: func(c *v1alpha1.CrawlSource) {
				c.Sitemap, c.ScopeExcludeRx, c.Title = "https://a/sitemap.xml", "/private/", "A site"
			},
			want: append(slices.Clone(base), "--useSitemap", "https://a/sitemap.xml", "--scopeExcludeRx", "/private/", "--title", "A site"),
		},
		{
			name:   "custom css",
			mutate: func(c *v1alpha1.CrawlSource) { c.CustomCSS = ".banner{display:none}" },
			want:   append(slices.Clone(base), "--custom-css", "/css/custom.css"),
		},
		{
			name:   "zero timeout is the 48h default",
			mutate: func(c *v1alpha1.CrawlSource) { c.Timeout = metav1.Duration{} },
			want:   base,
		},
		{
			name:   "timeout in seconds",
			mutate: func(c *v1alpha1.CrawlSource) { c.Timeout = metav1.Duration{Duration: 90 * time.Minute} },
			want:   append(slices.Clone(base[:len(base)-1]), "5400"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			za := testCrawlArchive()
			tt.mutate(za.Spec.Source.Crawl)
			zimit, _, _ := buildTestJob(za)

			assert.Equal(t, "zimit", zimit.Name)
			assert.Equal(t, testZimitImage, zimit.Image)
			assert.Equal(t, []string{"zimit"}, zimit.Command)
			assert.Equal(t, tt.want, zimit.Args)
			assert.NotContains(t, zimit.Args, "--keep")
		})
	}
}

func TestCrawlJobSecurity(t *testing.T) {
	job, _ := buildCrawlJob(testCrawlArchive(), testLib(), testAttempt, testZimitImage, testImage)
	zimit, upload, spec := buildTestJob(testCrawlArchive())

	assert.Equal(t, "site-abcdefgh", job.Name)
	assert.Equal(t, "kiwix", job.Namespace)
	assert.Equal(t, new(int32(0)), job.Spec.BackoffLimit)
	assert.Equal(t, new(int32(3600)), job.Spec.TTLSecondsAfterFinished)
	assert.Equal(t, new(int64(172800+21600)), job.Spec.ActiveDeadlineSeconds)

	require.NotNil(t, spec.SecurityContext)
	assert.Equal(t, new(int64(1000)), spec.SecurityContext.FSGroup)
	assert.Equal(t, new(false), spec.AutomountServiceAccountToken)
	assert.Equal(t, corev1.RestartPolicyNever, spec.RestartPolicy)

	zsc := zimit.SecurityContext
	require.NotNil(t, zsc)
	assert.Equal(t, []corev1.Capability{"ALL"}, zsc.Capabilities.Drop)
	assert.Equal(t, zimitExtraCapabilities, zsc.Capabilities.Add)
	assert.Empty(t, zsc.Capabilities.Add, "docs/notes/zimit-locked-down.md: zimit needs no capability")
	assert.Equal(t, new(false), zsc.AllowPrivilegeEscalation)
	assert.Equal(t, new(false), zsc.Privileged)
	assert.Equal(t, &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}, zsc.SeccompProfile)
	assert.Equal(t, corev1.TerminationMessageFallbackToLogsOnError, zimit.TerminationMessagePolicy)

	assert.Equal(t, "upload", upload.Name)
	assert.Equal(t, testImage, upload.Image)
	require.NotEmpty(t, upload.Args)
	assert.Equal(t, "upload", upload.Args[0])
	usc := upload.SecurityContext
	require.NotNil(t, usc)
	assert.Equal(t, new(int64(1000)), usc.RunAsUser)
	assert.Equal(t, new(true), usc.RunAsNonRoot)
	assert.Equal(t, new(false), usc.AllowPrivilegeEscalation)
	assert.Equal(t, []corev1.Capability{"ALL"}, usc.Capabilities.Drop)
	assert.Equal(t, &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}, usc.SeccompProfile)
	assert.Equal(t, corev1.TerminationMessageFallbackToLogsOnError, upload.TerminationMessagePolicy)
	// 5h of backoff budget plus 5m of the last wait leaves ~55m of transfer inside the 6h post-crawl window.
	assert.Contains(t, upload.Args, "--budget=5h")
	assert.Contains(t, upload.Args, "--max-backoff=5m")
}

func TestCrawlJobZeroTimeoutDeadline(t *testing.T) {
	za := testCrawlArchive()
	za.Spec.Source.Crawl.Timeout = metav1.Duration{}
	job, _ := buildCrawlJob(za, testLib(), testAttempt, testZimitImage, testImage)
	assert.Equal(t, new(int64(172800+21600)), job.Spec.ActiveDeadlineSeconds)
}

func TestCrawlJobVolumes(t *testing.T) {
	za := testCrawlArchive()
	za.Spec.Source.Crawl.Scratch.StorageClassName = new("fast")
	za.Spec.Source.Crawl.CustomCSS = ".banner{display:none}"
	zimit, upload, spec := buildTestJob(za)

	out := volume(t, spec, mountAt(t, zimit, "/output").Name)
	assert.Equal(t, out.Name, mountAt(t, upload, "/output").Name)
	require.NotNil(t, out.Ephemeral)
	claim := out.Ephemeral.VolumeClaimTemplate
	require.NotNil(t, claim)
	assert.Equal(t, resource.MustParse("20Gi"), claim.Spec.Resources.Requests[corev1.ResourceStorage])
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes)
	assert.Equal(t, new("fast"), claim.Spec.StorageClassName)
	assert.Equal(t, v1alpha1.ManagedByValue, claim.Labels[v1alpha1.LabelManagedBy])

	shm := volume(t, spec, mountAt(t, zimit, "/dev/shm").Name)
	require.NotNil(t, shm.EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, shm.EmptyDir.Medium)

	tok := mountAt(t, upload, "/var/run/kiwix")
	assert.True(t, tok.ReadOnly)
	secret := volume(t, spec, tok.Name)
	require.NotNil(t, secret.Secret)
	assert.Equal(t, "site-abcdefgh-upload", secret.Secret.SecretName)
	assert.Equal(t, []corev1.KeyToPath{{Key: "token", Path: "token"}}, secret.Secret.Items)
	for _, m := range zimit.VolumeMounts {
		assert.NotEqual(t, tok.Name, m.Name, "the upload token must not reach zimit")
	}

	css := volume(t, spec, mountAt(t, zimit, "/css").Name)
	require.NotNil(t, css.ConfigMap)
	assert.Equal(t, "site-abcdefgh-css", css.ConfigMap.Name)
	assert.Equal(t, []corev1.KeyToPath{{Key: "custom.css", Path: "custom.css"}}, css.ConfigMap.Items)
}

func TestCrawlJobNoCSSVolumeWithoutCustomCSS(t *testing.T) {
	job, cm := buildCrawlJob(testCrawlArchive(), testLib(), testAttempt, testZimitImage, testImage)
	assert.Nil(t, cm)
	for _, v := range job.Spec.Template.Spec.Volumes {
		assert.Nil(t, v.ConfigMap, "volume %s", v.Name)
	}
}

func TestCrawlJobLabelsAndEnv(t *testing.T) {
	za := testCrawlArchive()
	job, _ := buildCrawlJob(za, testLib(), testAttempt, testZimitImage, testImage)
	tmpl := job.Spec.Template

	for k, v := range map[string]string{
		v1alpha1.LabelLibrary: "wiki", v1alpha1.LabelArchive: "site", v1alpha1.LabelAttempt: testAttempt,
	} {
		assert.Equal(t, v, tmpl.Labels[k], k)
	}
	assert.NotContains(t, tmpl.Labels, labelName, "the kiwix Service selector must not match crawl pods")

	upload := tmpl.Spec.Containers[0]
	assert.Equal(t, []corev1.EnvVar{
		{Name: "LIBRARIAN_URL", Value: "http://wiki-librarian.kiwix.svc:8090"},
		{Name: "ATTEMPT", Value: testAttempt},
	}, upload.Env)
	assert.Equal(t, []string{"upload", "--output=/output", "--budget=5h", "--max-backoff=5m"}, upload.Args)

	za.Spec.Source.Crawl.KeepWarcs = true
	job, _ = buildCrawlJob(za, testLib(), testAttempt, testZimitImage, testImage)
	assert.Contains(t, job.Spec.Template.Spec.Containers[0].Args, "--keep-warcs")
}

func TestAttemptIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[a-z2-7]{8}$`)
	seen := map[string]bool{}
	for range 1000 {
		id := newAttemptID()
		if !re.MatchString(id) {
			t.Errorf("attempt ID %q does not match %s", id, re)
		}
		seen[id] = true
	}
	assert.Greater(t, len(seen), 990, "attempt IDs should be random")
}

func TestJobName(t *testing.T) {
	assert.Equal(t, "site-abcdefgh", jobName("site", "abcdefgh"))
}

func TestCrawlObjectsLabelled(t *testing.T) {
	za := testCrawlArchive()
	za.Spec.Source.Crawl.CustomCSS = ".banner{display:none}"
	job, cm := buildCrawlJob(za, testLib(), testAttempt, testZimitImage, testImage)
	require.NotNil(t, cm)
	secret := buildUploadSecret(za, testAttempt, "tok")

	for _, obj := range []metav1.Object{job, &job.Spec.Template, cm, secret} {
		assert.Equal(t, v1alpha1.ManagedByValue, obj.GetLabels()[v1alpha1.LabelManagedBy], "%T", obj)
	}
	assert.Equal(t, "site-abcdefgh-css", cm.Name)
	assert.Equal(t, ".banner{display:none}", cm.Data["custom.css"])
	assert.Equal(t, "site-abcdefgh-upload", secret.Name)
	assert.Equal(t, []byte("tok"), secret.Data["token"])
}

func TestCrawlJobEmptyScratchClassIsUnset(t *testing.T) {
	za := testCrawlArchive()
	za.Spec.Source.Crawl.Scratch.StorageClassName = new("")
	_, _, spec := buildTestJob(za)

	var claim *corev1.PersistentVolumeClaimTemplate
	for _, v := range spec.Volumes {
		if v.Ephemeral != nil {
			claim = v.Ephemeral.VolumeClaimTemplate
		}
	}

	require.NotNil(t, claim)
	assert.Nil(t, claim.Spec.StorageClassName, "an empty class must not be sent as \"\", which binds no claim")
}
