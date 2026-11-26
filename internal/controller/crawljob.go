package controller

import (
	"crypto/rand"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

// zimitExtraCapabilities is empty: zimit crawls with every capability dropped (docs/notes/zimit-locked-down.md).
var zimitExtraCapabilities []corev1.Capability

const (
	// defaultCrawlTimeout is the CRD default, also used for a 0s timeout, which admission allows.
	defaultCrawlTimeout = 48 * time.Hour
	// postCrawlWindow is the Job time left after the crawl for ZIM conversion and upload.
	postCrawlWindow = 6 * time.Hour
	// crawlJobTTL frees a finished Job's scratch volume within an hour.
	crawlJobTTL = 3600

	outputDir      = "/output"
	kiwixRunDir    = "/var/run/kiwix"
	cssDir         = "/css"
	cssKey         = "custom.css"
	uploadTokenKey = "token"
	librarianEnv   = "LIBRARIAN_URL"
	attemptEnv     = "ATTEMPT"
)

// newAttemptID returns 8 random characters from [a-z2-7].
func newAttemptID() string {
	return strings.ToLower(rand.Text()[:8])
}

// jobName is the crawl Job of an attempt, at most 54 characters for a 45-character archive name.
func jobName(archive, attempt string) string { return archive + "-" + attempt }

func uploadSecretName(archive, attempt string) string { return jobName(archive, attempt) + "-upload" }
func cssConfigMapName(archive, attempt string) string { return jobName(archive, attempt) + "-css" }

// crawlTimeout is spec.crawl.timeout, with 0 read as the 48h default.
func crawlTimeout(c *v1alpha1.CrawlSource) time.Duration {
	if c.Timeout.Duration <= 0 {
		return defaultCrawlTimeout
	}
	return c.Timeout.Duration
}

// crawlJobDeadline is the Job's activeDeadline: the crawl timeout plus the post-crawl window.
func crawlJobDeadline(c *v1alpha1.CrawlSource) time.Duration {
	return crawlTimeout(c) + postCrawlWindow
}

// crawlLabels are on every object of a crawl attempt; the library's crawl NetworkPolicy selects pods by LabelLibrary.
func crawlLabels(za *v1alpha1.ZimArchive, attempt string) map[string]string {
	return map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelLibrary:   za.Spec.LibraryRef.Name,
		v1alpha1.LabelArchive:   za.Name,
		v1alpha1.LabelAttempt:   attempt,
	}
}

func crawlMeta(za *v1alpha1.ZimArchive, name, attempt string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: za.Namespace, Labels: crawlLabels(za, attempt)}
}

// buildUploadSecret holds an attempt's upload token, mounted only into the upload container.
func buildUploadSecret(za *v1alpha1.ZimArchive, attempt, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: crawlMeta(za, uploadSecretName(za.Name, attempt), attempt),
		Data:       map[string][]byte{uploadTokenKey: []byte(token)},
	}
}

func zimitArgs(za *v1alpha1.ZimArchive) []string {
	c := za.Spec.Source.Crawl
	args := []string{
		"--seeds", strings.Join(c.Seeds, ","),
		"--name", za.Name,
		"--output", outputDir,
		"--build", outputDir + "/build",
		"--workers", strconv.Itoa(int(c.Workers)),
		"--timeSoftLimit", strconv.FormatInt(int64(crawlTimeout(c)/time.Second), 10),
	}
	for _, opt := range []struct{ flag, value string }{
		{"--useSitemap", c.Sitemap}, {"--scopeExcludeRx", c.ScopeExcludeRx}, {"--title", c.Title},
	} {
		if opt.value != "" {
			args = append(args, opt.flag, opt.value)
		}
	}
	if c.CustomCSS != "" {
		args = append(args, "--custom-css", cssDir+"/"+cssKey)
	}
	return args
}

// buildCrawlJob returns an attempt's Job, zimit as the init container and the uploader after it, and its customCss ConfigMap, nil without customCss. Owner references are the caller's.
func buildCrawlJob(za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, attempt, zimitImage, operatorImage string) (*batchv1.Job, *corev1.ConfigMap) {
	c := za.Spec.Source.Crawl
	name := jobName(za.Name, attempt)
	output := corev1.VolumeMount{Name: "output", MountPath: outputDir}

	zimit := corev1.Container{
		Name:    "zimit",
		Image:   zimitImage,
		Command: []string{"zimit"},
		Args:    zimitArgs(za),
		VolumeMounts: []corev1.VolumeMount{
			output,
			{Name: "dshm", MountPath: "/dev/shm"},
		},
		Resources:                c.Resources,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		// zimit runs as root, so it gets no capability, no escalation and the runtime's seccomp profile.
		SecurityContext: &corev1.SecurityContext{
			Privileged:               new(false),
			AllowPrivilegeEscalation: new(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: zimitExtraCapabilities},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}

	uploadArgs := []string{"upload", "--output=" + outputDir, "--budget=5h", "--max-backoff=5m"}
	if c.KeepWarcs {
		uploadArgs = append(uploadArgs, "--keep-warcs")
	}
	uploadSC := lockedDown()
	uploadSC.RunAsUser = new(int64(kiwixUID))
	uploadSC.RunAsGroup = new(int64(kiwixUID))
	uploadSC.RunAsNonRoot = new(true)
	uploadSC.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	upload := corev1.Container{
		Name:  "upload",
		Image: operatorImage,
		Args:  uploadArgs,
		Env: []corev1.EnvVar{
			{Name: librarianEnv, Value: librarianURL(lib)},
			{Name: attemptEnv, Value: attempt},
		},
		VolumeMounts:             []corev1.VolumeMount{output, {Name: "token", MountPath: kiwixRunDir, ReadOnly: true}},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext:          uploadSC,
	}

	volumes := []corev1.Volume{
		{Name: "output", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{Labels: crawlLabels(za, attempt)},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: c.Scratch.Size}},
					StorageClassName: c.Scratch.StorageClassName,
				},
			},
		}}},
		// Chromium needs more shared memory than the 64Mi default.
		{Name: "dshm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		{Name: "token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: uploadSecretName(za.Name, attempt),
			Items:      []corev1.KeyToPath{{Key: uploadTokenKey, Path: uploadTokenKey}},
		}}},
	}

	var cm *corev1.ConfigMap
	if c.CustomCSS != "" {
		cm = &corev1.ConfigMap{
			ObjectMeta: crawlMeta(za, cssConfigMapName(za.Name, attempt), attempt),
			Data:       map[string]string{cssKey: c.CustomCSS},
		}
		zimit.VolumeMounts = append(zimit.VolumeMounts, corev1.VolumeMount{Name: "css", MountPath: cssDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "css", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name},
			Items:                []corev1.KeyToPath{{Key: cssKey, Path: cssKey}},
		}}})
	}

	job := &batchv1.Job{
		ObjectMeta: crawlMeta(za, name, attempt),
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(0)),
			TTLSecondsAfterFinished: new(int32(crawlJobTTL)),
			ActiveDeadlineSeconds:   new(int64(crawlJobDeadline(c) / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: crawlLabels(za, attempt)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: new(false),
					SecurityContext:              &corev1.PodSecurityContext{FSGroup: new(int64(kiwixUID))},
					InitContainers:               []corev1.Container{zimit},
					Containers:                   []corev1.Container{upload},
					Volumes:                      volumes,
				},
			},
		},
	}
	return job, cm
}
