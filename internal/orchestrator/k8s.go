package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/config"
)

const (
	serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	serviceAccountCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	workerConfigMountPath   = "/var/run/flow/worker.yaml"
	harnessConfigMountPath  = "/var/run/flow/harness.json"
	// maxHarnessConfigBytes bounds the orchestrator-supplied Harness config so
	// the per-worker Secret stays well under the Kubernetes 1 MiB object limit.
	maxHarnessConfigBytes = 1 << 20
	genericWorkVolumeName = "work"
	// The Job controller appends '-' and five generated characters to a Pod name.
	jobPodGeneratedSuffixLength = 6
)

// KubernetesProviderOptions configures a stdlib-only Kubernetes REST provider.
type KubernetesProviderOptions struct {
	BaseURL                     string
	Token                       string
	TokenFile                   string
	HTTPClient                  *http.Client
	Namespace                   string
	Image                       string
	ServiceAccount              string
	WorkDir                     string
	WorkerArgs                  []string
	ImagePullPolicy             string
	HarnessModelProxySecretName string
	HarnessConfigFile           string
	WorkVolume                  *config.OrchestratorWorkVolumeConfig
	Resources                   *config.OrchestratorResourceRequirements
	NodeSelector                map[string]string
	DeletionTimeout             time.Duration
}

// KubernetesProvider creates one Secret and one batch/v1 Job per one-shot slot.
type KubernetesProvider struct {
	baseURL                     string
	token                       string
	tokenFile                   string
	client                      *http.Client
	namespace                   string
	image                       string
	serviceAccount              string
	workDir                     string
	workerArgs                  []string
	imagePullPolicy             string
	harnessModelProxySecretName string
	harnessConfigFile           string
	workVolume                  *config.OrchestratorWorkVolumeConfig
	resources                   *config.OrchestratorResourceRequirements
	nodeSelector                map[string]string
	deletionTimeout             time.Duration
}

// NewKubernetesProvider constructs a provider for an explicit API endpoint.
func NewKubernetesProvider(options KubernetesProviderOptions) (*KubernetesProvider, error) {
	options.BaseURL = strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	options.Namespace = strings.TrimSpace(options.Namespace)
	if options.BaseURL == "" {
		return nil, errors.New("kubernetes API base URL is required")
	}
	if options.Namespace == "" {
		return nil, errors.New("kubernetes namespace is required")
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if options.DeletionTimeout <= 0 {
		options.DeletionTimeout = 2 * time.Minute
	}
	return &KubernetesProvider{
		baseURL: options.BaseURL, token: strings.TrimSpace(options.Token), tokenFile: strings.TrimSpace(options.TokenFile), client: options.HTTPClient,
		namespace: options.Namespace, image: strings.TrimSpace(options.Image),
		serviceAccount: strings.TrimSpace(options.ServiceAccount), workDir: strings.TrimSpace(options.WorkDir),
		workerArgs: append([]string(nil), options.WorkerArgs...), imagePullPolicy: strings.TrimSpace(options.ImagePullPolicy),
		harnessModelProxySecretName: strings.TrimSpace(options.HarnessModelProxySecretName),
		harnessConfigFile:           strings.TrimSpace(options.HarnessConfigFile),
		workVolume:                  cloneKubernetesWorkVolume(options.WorkVolume), resources: cloneKubernetesResources(options.Resources),
		nodeSelector: cloneKubernetesStringMap(options.NodeSelector), deletionTimeout: options.DeletionTimeout,
	}, nil
}

// NewInClusterKubernetesProvider reads the mounted service-account token and CA
// and discovers the Kubernetes API from the standard in-cluster environment.
func NewInClusterKubernetesProvider(options KubernetesProviderOptions) (*KubernetesProvider, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if host == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is not set: orchestrator must run in a Kubernetes cluster")
	}
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if port == "" {
		port = "443"
	}
	token, err := os.ReadFile(serviceAccountTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service-account token: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, errors.New("Kubernetes service-account token is empty")
	}
	caPEM, err := os.ReadFile(serviceAccountCAPath)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service-account CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse Kubernetes service-account CA %s: no certificates", serviceAccountCAPath)
	}
	options.BaseURL = "https://" + net.JoinHostPort(host, port)
	options.Token = strings.TrimSpace(string(token))
	options.TokenFile = serviceAccountTokenPath
	options.HTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	return NewKubernetesProvider(options)
}

// KubernetesResourceNames returns stable DNS-safe Job and Secret names.
func KubernetesResourceNames(identity AssignmentIdentity) (jobName, secretName string) {
	slug := dnsSlug(identity.AssignmentID)
	if len(slug) > 26 {
		slug = slug[:26]
		slug = strings.TrimRight(slug, "-")
	}
	hash := identityHash(identity)[:12]
	jobName = "flow-worker-" + slug + "-" + hash
	secretName = jobName + "-config"
	return jobName, secretName
}

func (k *KubernetesProvider) Health(ctx context.Context, profile Profile) error {
	settings, err := k.settings(profile)
	if err != nil {
		return Permanent(err)
	}
	if _, err := k.request(ctx, http.MethodGet, k.jobsPath(settings.namespace)+"?limit=1", nil, &struct{}{}); err != nil {
		return fmt.Errorf("list worker Jobs in namespace %s: %w", settings.namespace, err)
	}
	return nil
}

func (k *KubernetesProvider) Launch(ctx context.Context, request LaunchRequest) error {
	settings, err := k.settings(request.Profile)
	if err != nil {
		return Permanent(err)
	}
	if strings.TrimSpace(request.WorkerToken) == "" {
		return Permanent(errors.New("worker token is required"))
	}
	config, err := generatedWorkerYAML(request, settings.workDir, settings.harnessWorkerConfigPath())
	if err != nil {
		return Permanent(fmt.Errorf("generate worker config: %w", err))
	}
	secretData := map[string][]byte{"worker.yaml": config}
	if settings.harnessConfigFile != "" {
		// The Harness config may contain credentials: it travels only inside
		// the existing private per-worker Secret and is never logged.
		harnessConfig, err := os.ReadFile(settings.harnessConfigFile)
		if err != nil {
			return Permanent(fmt.Errorf("read harness config file: %w", err))
		}
		if len(harnessConfig) > maxHarnessConfigBytes {
			return Permanent(fmt.Errorf("read harness config file: exceeds %d bytes", maxHarnessConfigBytes))
		}
		secretData["harness.json"] = harnessConfig
	}
	jobName, secretName := KubernetesResourceNames(request.Identity)
	labels, annotations := kubernetesIdentityMetadata(request.Identity)
	secret := kubernetesSecret{
		APIVersion: "v1", Kind: "Secret",
		Metadata: kubernetesMetadata{Name: secretName, Namespace: settings.namespace, Labels: labels, Annotations: annotations},
		Type:     "Opaque", Data: secretData,
	}
	if err := k.createOrReplaceOwnedSecret(ctx, settings.namespace, secret, request.Identity); err != nil {
		return fmt.Errorf("create worker Secret %s: %w", secretName, err)
	}
	mode := int32(0o400)
	automountToken := false
	enableServiceLinks := false
	runAsNonRoot := true
	runAsUser := int64(1000)
	runAsGroup := int64(1000)
	fsGroup := int64(1000)
	command := []string{"flow-worker", "run", "--one-shot", "--config", workerConfigMountPath}
	command = append(command, settings.workerArgs...)
	job := kubernetesJob{
		APIVersion: "batch/v1", Kind: "Job",
		Metadata: kubernetesMetadata{Name: jobName, Namespace: settings.namespace, Labels: labels, Annotations: annotations},
	}
	job.Spec.BackoffLimit = 0
	job.Spec.Template.Metadata.Labels = labels
	job.Spec.Template.Metadata.Annotations = annotations
	job.Spec.Template.Spec.RestartPolicy = "Never"
	job.Spec.Template.Spec.ServiceAccountName = settings.serviceAccount
	job.Spec.Template.Spec.AutomountServiceAccountToken = &automountToken
	job.Spec.Template.Spec.EnableServiceLinks = &enableServiceLinks
	job.Spec.Template.Spec.NodeSelector = cloneKubernetesStringMap(settings.nodeSelector)
	job.Spec.Template.Spec.SecurityContext = kubernetesPodSecurityContext{
		RunAsNonRoot: &runAsNonRoot, RunAsUser: &runAsUser, RunAsGroup: &runAsGroup, FSGroup: &fsGroup, SeccompProfile: kubernetesSeccompProfile{Type: "RuntimeDefault"},
	}
	workVolumeName := "worker-work" // Preserve the exact legacy manifest when work_volume is omitted.
	workVolume := kubernetesVolume{Name: workVolumeName, EmptyDir: &kubernetesEmptyDirVolume{}}
	if settings.workVolume != nil {
		workVolumeName = genericWorkVolumeName // Keeps <Job pod name>-<volume name> within the 63-character DNS label invariant.
		workVolume = kubernetesVolume{Name: workVolumeName}
		switch settings.workVolume.Type {
		case "empty_dir":
			workVolume.EmptyDir = &kubernetesEmptyDirVolume{SizeLimit: settings.workVolume.SizeLimit}
		case "generic_ephemeral":
			workVolume.Ephemeral = &kubernetesEphemeralVolume{VolumeClaimTemplate: kubernetesPersistentVolumeClaimTemplate{
				Metadata: kubernetesMetadata{Labels: cloneKubernetesStringMap(labels)},
				Spec: kubernetesPersistentVolumeClaimSpec{
					AccessModes: append([]string(nil), settings.workVolume.AccessModes...), StorageClassName: settings.workVolume.StorageClassName,
					Resources: kubernetesResourceRequirements{Requests: map[string]string{"storage": settings.workVolume.Size}},
				},
			}}
		}
	}
	job.Spec.Template.Spec.Containers = []kubernetesContainer{{
		Name: "worker", Image: settings.image, ImagePullPolicy: settings.imagePullPolicy, Command: command,
		Env: harnessModelProxyEnvironment(settings.harnessModelProxySecretName), Resources: kubernetesResources(settings.resources),
		VolumeMounts: []kubernetesVolumeMount{
			{Name: "worker-config", MountPath: "/var/run/flow", ReadOnly: true},
			{Name: workVolumeName, MountPath: kubernetesWorkVolumeMountPath(settings.workVolume, settings.workDir)},
		},
	}}
	job.Spec.Template.Spec.Volumes = []kubernetesVolume{
		{Name: "worker-config", Secret: &kubernetesSecretVolume{SecretName: secretName, DefaultMode: &mode}},
		workVolume,
	}
	if err := k.createOrVerifyOwnedJob(ctx, settings.namespace, job, request.Identity); err != nil {
		// Leave the owned Secret as an inspectable incomplete resource. The
		// reconciler fences its credential before deleting it during retry or
		// permanent-failure abandonment.
		return fmt.Errorf("create worker Job %s: %w", jobName, err)
	}
	return nil
}

func (k *KubernetesProvider) Inspect(ctx context.Context, identity AssignmentIdentity) (ProviderStatus, error) {
	jobName, secretName := KubernetesResourceNames(identity)
	namespace := k.namespaceFor(identity)
	path := k.jobsPath(namespace) + "/" + url.PathEscape(jobName)
	var job kubernetesJobStatus
	status, err := k.request(ctx, http.MethodGet, path, nil, &job)
	if status == http.StatusNotFound {
		secretPath := k.secretsPath(namespace) + "/" + url.PathEscape(secretName)
		var secret kubernetesSecret
		secretStatus, secretErr := k.request(ctx, http.MethodGet, secretPath, nil, &secret)
		if secretErr != nil {
			return "", fmt.Errorf("inspect worker Secret %s: %w", secretName, secretErr)
		}
		if secretStatus == http.StatusNotFound {
			return ProviderNotFound, nil
		}
		if err := requireKubernetesOwnership(secret.Metadata, identity); err != nil {
			return "", Permanent(fmt.Errorf("inspect worker Secret %s: %w", secretName, err))
		}
		return ProviderIncomplete, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect worker Job %s: %w", jobName, err)
	}
	if err := requireKubernetesOwnership(job.Metadata, identity); err != nil {
		return "", Permanent(fmt.Errorf("inspect worker Job %s: %w", jobName, err))
	}
	for _, condition := range job.Status.Conditions {
		if strings.EqualFold(condition.Status, "True") {
			switch condition.Type {
			case "Failed":
				return ProviderFailed, nil
			case "Complete":
				return ProviderSucceeded, nil
			}
		}
	}
	if job.Status.Failed > 0 {
		return ProviderFailed, nil
	}
	if job.Status.Succeeded > 0 {
		return ProviderSucceeded, nil
	}
	if job.Status.Active > 0 {
		return ProviderRunning, nil
	}
	return ProviderPending, nil
}

func (k *KubernetesProvider) Delete(ctx context.Context, identity AssignmentIdentity) error {
	jobName, secretName := KubernetesResourceNames(identity)
	namespace := k.namespaceFor(identity)
	jobPath := k.jobsPath(namespace) + "/" + url.PathEscape(jobName)
	var job kubernetesJob
	status, err := k.request(ctx, http.MethodGet, jobPath, nil, &job)
	if err != nil {
		return fmt.Errorf("inspect worker Job %s before deletion: %w", jobName, err)
	}
	if status != http.StatusNotFound {
		if err := requireKubernetesOwnership(job.Metadata, identity); err != nil {
			return Permanent(fmt.Errorf("refuse to delete worker Job %s: %w", jobName, err))
		}
		body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground"}
		if _, err := k.request(ctx, http.MethodDelete, jobPath, body, nil); err != nil {
			return fmt.Errorf("delete worker Job %s: %w", jobName, err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, k.deletionTimeout)
		err := k.waitForDeletion(waitCtx, jobPath)
		cancel()
		if err != nil {
			return fmt.Errorf("wait for worker Job %s deletion: %w", jobName, err)
		}
	}
	if err := k.deleteOwnedSecret(ctx, namespace, secretName, identity); err != nil {
		return fmt.Errorf("delete worker Secret %s: %w", secretName, err)
	}
	return nil
}

type kubernetesSettings struct {
	namespace, image, serviceAccount, workDir, imagePullPolicy string
	harnessModelProxySecretName, harnessConfigFile             string
	workerArgs                                                 []string
	workVolume                                                 *config.OrchestratorWorkVolumeConfig
	resources                                                  *config.OrchestratorResourceRequirements
	nodeSelector                                               map[string]string
}

// harnessWorkerConfigPath is the path of the Secret-mounted Harness config the
// worker installs as each job's global config; empty when unconfigured.
func (s kubernetesSettings) harnessWorkerConfigPath() string {
	if s.harnessConfigFile == "" {
		return ""
	}
	return harnessConfigMountPath
}

func (k *KubernetesProvider) settings(profile Profile) (kubernetesSettings, error) {
	persisted := config.OrchestratorKubernetesConfig{
		Namespace: k.namespace, Image: k.image, ServiceAccount: k.serviceAccount, WorkDir: k.workDir,
		ImagePullPolicy: k.imagePullPolicy, HarnessModelProxySecretName: k.harnessModelProxySecretName,
		HarnessConfigFile: k.harnessConfigFile,
		WorkerArgs:        append([]string(nil), k.workerArgs...), WorkVolume: cloneKubernetesWorkVolume(k.workVolume),
		Resources: cloneKubernetesResources(k.resources), NodeSelector: cloneKubernetesStringMap(k.nodeSelector),
	}
	if hasKubernetesProviderDescriptor(profile.ProviderOptions) {
		for _, key := range []string{"namespace", "image", "work_dir"} {
			if strings.TrimSpace(profile.ProviderOptions[key]) == "" {
				return kubernetesSettings{}, fmt.Errorf("invalid persisted Kubernetes provider option %s: value is required", key)
			}
		}
		persisted = config.OrchestratorKubernetesConfig{
			Namespace: profile.ProviderOptions["namespace"], Image: profile.ProviderOptions["image"],
			ServiceAccount: profile.ProviderOptions["service_account"], WorkDir: profile.ProviderOptions["work_dir"],
			ImagePullPolicy:             profile.ProviderOptions["image_pull_policy"],
			HarnessModelProxySecretName: profile.ProviderOptions["harness_model_proxy_secret_name"],
			HarnessConfigFile:           profile.ProviderOptions["harness_config_file"],
		}
	}
	if err := decodeKubernetesProviderOption(profile.ProviderOptions, "worker_args", &persisted.WorkerArgs, false); err != nil {
		return kubernetesSettings{}, err
	}
	if err := decodeKubernetesProviderOption(profile.ProviderOptions, "work_volume", &persisted.WorkVolume, true); err != nil {
		return kubernetesSettings{}, err
	}
	if err := decodeKubernetesProviderOption(profile.ProviderOptions, "resources", &persisted.Resources, true); err != nil {
		return kubernetesSettings{}, err
	}
	if err := decodeKubernetesProviderOption(profile.ProviderOptions, "node_selector", &persisted.NodeSelector, true); err != nil {
		return kubernetesSettings{}, err
	}
	resolved, err := config.ResolveOrchestratorKubernetesConfig(persisted)
	if err != nil {
		return kubernetesSettings{}, fmt.Errorf("invalid Kubernetes provider options: %w", err)
	}
	if err := validateWorkerArgs(resolved.WorkerArgs); err != nil {
		return kubernetesSettings{}, err
	}
	serviceAccount := resolved.ServiceAccount
	if serviceAccount == "" {
		serviceAccount = "default"
	}
	return kubernetesSettings{
		namespace: resolved.Namespace, image: resolved.Image, serviceAccount: serviceAccount,
		workDir: resolved.WorkDir, imagePullPolicy: resolved.ImagePullPolicy,
		harnessModelProxySecretName: resolved.HarnessModelProxySecretName, harnessConfigFile: resolved.HarnessConfigFile,
		workerArgs: resolved.WorkerArgs,
		workVolume: resolved.WorkVolume, resources: resolved.Resources, nodeSelector: resolved.NodeSelector,
	}, nil
}

func hasKubernetesProviderDescriptor(options map[string]string) bool {
	for _, key := range []string{
		"namespace", "image", "service_account", "work_dir", "image_pull_policy", "harness_model_proxy_secret_name",
		"harness_config_file", "worker_args", "work_volume", "resources", "node_selector",
	} {
		if _, ok := options[key]; ok {
			return true
		}
	}
	return false
}

func kubernetesWorkVolumeMountPath(volume *config.OrchestratorWorkVolumeConfig, workDir string) string {
	if volume == nil {
		return workDir
	}
	return volume.MountPath
}

func decodeKubernetesProviderOption(options map[string]string, key string, target any, rejectNull bool) error {
	raw, exists := options[key]
	if !exists {
		return nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || rejectNull && raw == "null" {
		return fmt.Errorf("provider option %s is empty or null", key)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("provider option %s is invalid JSON: %w", key, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("provider option %s has trailing JSON data", key)
	}
	return nil
}

func harnessModelProxyEnvironment(secretName string) []kubernetesEnvVar {
	if secretName == "" {
		return nil
	}
	return []kubernetesEnvVar{
		{Name: "HARNESS_MODEL_PROXY_URL", ValueFrom: kubernetesEnvVarSource{SecretKeyRef: kubernetesSecretKeySelector{Name: secretName, Key: "HARNESS_MODEL_PROXY_URL"}}},
		{Name: "HARNESS_MODEL_PROXY_API_KEY", ValueFrom: kubernetesEnvVarSource{SecretKeyRef: kubernetesSecretKeySelector{Name: secretName, Key: "HARNESS_MODEL_PROXY_API_KEY"}}},
	}
}

func (k *KubernetesProvider) namespaceFor(identity AssignmentIdentity) string {
	if namespace := strings.TrimSpace(identity.ProviderOptions["namespace"]); namespace != "" {
		return namespace
	}
	return k.namespace
}

func option(profile Profile, key, fallback string) string {
	if value := strings.TrimSpace(profile.ProviderOptions[key]); value != "" {
		return value
	}
	return fallback
}

func cloneKubernetesStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	cloned := make(map[string]string, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func cloneKubernetesWorkVolume(value *config.OrchestratorWorkVolumeConfig) *config.OrchestratorWorkVolumeConfig {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.AccessModes = append([]string(nil), value.AccessModes...)
	return &cloned
}

func cloneKubernetesResources(value *config.OrchestratorResourceRequirements) *config.OrchestratorResourceRequirements {
	if value == nil {
		return nil
	}
	return &config.OrchestratorResourceRequirements{
		Requests: cloneKubernetesStringMap(value.Requests), Limits: cloneKubernetesStringMap(value.Limits),
	}
}

func kubernetesResources(value *config.OrchestratorResourceRequirements) *kubernetesResourceRequirements {
	if value == nil {
		return nil
	}
	return &kubernetesResourceRequirements{
		Requests: cloneKubernetesStringMap(value.Requests), Limits: cloneKubernetesStringMap(value.Limits),
	}
}

func (k *KubernetesProvider) createOrReplaceOwnedSecret(ctx context.Context, namespace string, secret kubernetesSecret, identity AssignmentIdentity) error {
	collection := k.secretsPath(namespace)
	status, err := k.request(ctx, http.MethodPost, collection, secret, nil)
	if status != http.StatusConflict {
		return err
	}
	path := collection + "/" + url.PathEscape(secret.Metadata.Name)
	var existing kubernetesSecret
	if _, getErr := k.request(ctx, http.MethodGet, path, nil, &existing); getErr != nil {
		return getErr
	}
	if ownErr := requireKubernetesOwnership(existing.Metadata, identity); ownErr != nil {
		return Permanent(ownErr)
	}
	if existing.Type != "Opaque" {
		return Permanent(fmt.Errorf("existing Secret %s has unexpected type %q", secret.Metadata.Name, existing.Type))
	}
	if strings.TrimSpace(existing.Metadata.ResourceVersion) == "" {
		return Permanent(fmt.Errorf("existing Secret %s has no resource version", secret.Metadata.Name))
	}
	secret.Metadata.ResourceVersion = existing.Metadata.ResourceVersion
	_, err = k.request(ctx, http.MethodPut, path, secret, nil)
	return err
}

func (k *KubernetesProvider) createOrVerifyOwnedJob(ctx context.Context, namespace string, job kubernetesJob, identity AssignmentIdentity) error {
	collection := k.jobsPath(namespace)
	status, err := k.request(ctx, http.MethodPost, collection, job, nil)
	if status != http.StatusConflict {
		return err
	}
	var existing kubernetesJob
	if _, getErr := k.request(ctx, http.MethodGet, collection+"/"+url.PathEscape(job.Metadata.Name), nil, &existing); getErr != nil {
		return getErr
	}
	if ownErr := requireKubernetesOwnership(existing.Metadata, identity); ownErr != nil {
		return Permanent(ownErr)
	}
	if !metadataContains(existing.Spec.Template.Metadata.Labels, job.Spec.Template.Metadata.Labels) ||
		!metadataContains(existing.Spec.Template.Metadata.Annotations, job.Spec.Template.Metadata.Annotations) ||
		!reflect.DeepEqual(existing.Spec.Template.Spec, job.Spec.Template.Spec) {
		return Permanent(fmt.Errorf("existing Job %s does not match the assignment worker spec", job.Metadata.Name))
	}
	return nil
}

func (k *KubernetesProvider) deleteOwnedSecret(ctx context.Context, namespace, name string, identity AssignmentIdentity) error {
	path := k.secretsPath(namespace) + "/" + url.PathEscape(name)
	var secret kubernetesSecret
	status, err := k.request(ctx, http.MethodGet, path, nil, &secret)
	if err != nil || status == http.StatusNotFound {
		return err
	}
	if err := requireKubernetesOwnership(secret.Metadata, identity); err != nil {
		return Permanent(fmt.Errorf("refuse to delete Secret %s: %w", name, err))
	}
	_, err = k.request(ctx, http.MethodDelete, path, map[string]any{"apiVersion": "v1", "kind": "DeleteOptions"}, nil)
	return err
}

func (k *KubernetesProvider) waitForDeletion(ctx context.Context, path string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := k.request(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		if status == http.StatusNotFound {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func requireKubernetesOwnership(metadata kubernetesMetadata, identity AssignmentIdentity) error {
	jobName, secretName := KubernetesResourceNames(identity)
	if metadata.Name != jobName && metadata.Name != secretName {
		return fmt.Errorf("resource name %q does not belong to assignment %s", metadata.Name, identity.AssignmentID)
	}
	labels, annotations := kubernetesIdentityMetadata(identity)
	if !metadataContains(metadata.Labels, labels) || !metadataContains(metadata.Annotations, annotations) {
		return fmt.Errorf("resource %s ownership metadata does not match assignment %s", metadata.Name, identity.AssignmentID)
	}
	return nil
}

func metadataContains(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func (k *KubernetesProvider) request(ctx context.Context, method, path string, body, target any) (int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode Kubernetes request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("build Kubernetes request: %w", err)
	}
	token := k.token
	if k.tokenFile != "" {
		data, err := os.ReadFile(k.tokenFile)
		if err != nil {
			return 0, fmt.Errorf("read Kubernetes service-account token: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return 0, errors.New("Kubernetes service-account token is empty")
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 4<<20)
	if resp.StatusCode == http.StatusNotFound && (method == http.MethodDelete || method == http.MethodGet) {
		_, _ = io.Copy(io.Discard, limited)
		return resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(limited)
		err := fmt.Errorf("Kubernetes API status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		if permanentHTTPStatus(resp.StatusCode) {
			err = Permanent(err)
		}
		return resp.StatusCode, err
	}
	if target != nil {
		if err := json.NewDecoder(limited).Decode(target); err != nil {
			return resp.StatusCode, fmt.Errorf("decode Kubernetes response: %w", err)
		}
	} else {
		_, _ = io.Copy(io.Discard, limited)
	}
	return resp.StatusCode, nil
}

func permanentHTTPStatus(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusConflict && status != http.StatusTooManyRequests
}

func (k *KubernetesProvider) jobsPath(namespace string) string {
	return "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs"
}
func (k *KubernetesProvider) secretsPath(namespace string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets"
}

func dnsSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "assignment"
	}
	return result
}

func identityHash(identity AssignmentIdentity) string {
	digest := sha256.Sum256([]byte(identity.AssignmentID + "\x00" + identity.WorkerID + "\x00" + identity.ProviderID + "\x00" + identity.ProfileName + "\x00" + identity.ProviderRequestID))
	return hex.EncodeToString(digest[:])
}

func kubernetesIdentityMetadata(identity AssignmentIdentity) (map[string]string, map[string]string) {
	profileLabel := dnsSlug(identity.ProfileName)
	if len(profileLabel) > 63 {
		profileLabel = strings.TrimRight(profileLabel[:54], "-") + "-" + identityHash(identity)[:8]
	}
	labels := map[string]string{
		"app.kubernetes.io/name":              "flow-worker",
		"app.kubernetes.io/managed-by":        "flow-orchestrator",
		"flow.clarifiedlabs.com/assignment":   identityHash(identity)[:24],
		"flow.clarifiedlabs.com/profile-name": profileLabel,
	}
	annotations := map[string]string{
		"flow.clarifiedlabs.com/assignment-id":       identity.AssignmentID,
		"flow.clarifiedlabs.com/provider-request-id": identity.ProviderRequestID,
		"flow.clarifiedlabs.com/worker-id":           identity.WorkerID,
		"flow.clarifiedlabs.com/provider-id":         identity.ProviderID,
		"flow.clarifiedlabs.com/profile-name":        identity.ProfileName,
	}
	return labels, annotations
}

type kubernetesMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}
type kubernetesSecret struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   kubernetesMetadata `json:"metadata"`
	Type       string             `json:"type"`
	Data       map[string][]byte  `json:"data"`
}
type kubernetesJob struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   kubernetesMetadata `json:"metadata"`
	Spec       struct {
		BackoffLimit int `json:"backoffLimit"`
		Template     struct {
			Metadata struct {
				Labels      map[string]string `json:"labels,omitempty"`
				Annotations map[string]string `json:"annotations,omitempty"`
			} `json:"metadata"`
			Spec struct {
				ServiceAccountName           string                       `json:"serviceAccountName,omitempty"`
				AutomountServiceAccountToken *bool                        `json:"automountServiceAccountToken,omitempty"`
				EnableServiceLinks           *bool                        `json:"enableServiceLinks,omitempty"`
				SecurityContext              kubernetesPodSecurityContext `json:"securityContext"`
				RestartPolicy                string                       `json:"restartPolicy"`
				Containers                   []kubernetesContainer        `json:"containers"`
				Volumes                      []kubernetesVolume           `json:"volumes"`
				NodeSelector                 map[string]string            `json:"nodeSelector,omitempty"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}
type kubernetesContainer struct {
	Name            string                          `json:"name"`
	Image           string                          `json:"image"`
	ImagePullPolicy string                          `json:"imagePullPolicy,omitempty"`
	Command         []string                        `json:"command"`
	Env             []kubernetesEnvVar              `json:"env,omitempty"`
	Resources       *kubernetesResourceRequirements `json:"resources,omitempty"`
	VolumeMounts    []kubernetesVolumeMount         `json:"volumeMounts"`
}
type kubernetesEnvVar struct {
	Name      string                 `json:"name"`
	ValueFrom kubernetesEnvVarSource `json:"valueFrom"`
}
type kubernetesEnvVarSource struct {
	SecretKeyRef kubernetesSecretKeySelector `json:"secretKeyRef"`
}
type kubernetesSecretKeySelector struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type kubernetesPodSecurityContext struct {
	RunAsNonRoot   *bool                    `json:"runAsNonRoot"`
	RunAsUser      *int64                   `json:"runAsUser"`
	RunAsGroup     *int64                   `json:"runAsGroup"`
	FSGroup        *int64                   `json:"fsGroup"`
	SeccompProfile kubernetesSeccompProfile `json:"seccompProfile"`
}
type kubernetesSeccompProfile struct {
	Type string `json:"type"`
}
type kubernetesVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}
type kubernetesVolume struct {
	Name      string                     `json:"name"`
	Secret    *kubernetesSecretVolume    `json:"secret,omitempty"`
	EmptyDir  *kubernetesEmptyDirVolume  `json:"emptyDir,omitempty"`
	Ephemeral *kubernetesEphemeralVolume `json:"ephemeral,omitempty"`
}
type kubernetesEmptyDirVolume struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}
type kubernetesEphemeralVolume struct {
	VolumeClaimTemplate kubernetesPersistentVolumeClaimTemplate `json:"volumeClaimTemplate"`
}
type kubernetesPersistentVolumeClaimTemplate struct {
	Metadata kubernetesMetadata                  `json:"metadata"`
	Spec     kubernetesPersistentVolumeClaimSpec `json:"spec"`
}
type kubernetesPersistentVolumeClaimSpec struct {
	AccessModes      []string                       `json:"accessModes"`
	StorageClassName string                         `json:"storageClassName,omitempty"`
	Resources        kubernetesResourceRequirements `json:"resources"`
}
type kubernetesResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}
type kubernetesSecretVolume struct {
	SecretName  string `json:"secretName"`
	DefaultMode *int32 `json:"defaultMode,omitempty"`
}
type kubernetesJobStatus struct {
	Metadata kubernetesMetadata `json:"metadata"`
	Status   struct {
		Active     int `json:"active"`
		Succeeded  int `json:"succeeded"`
		Failed     int `json:"failed"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}
