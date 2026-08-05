package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestKubernetesProviderExactJobSecretIdentityAndDelete(t *testing.T) {
	assignment := testAssignment("pending")
	identity := IdentityOf(assignment)
	jobName, secretName := KubernetesResourceNames(identity)
	var mu sync.Mutex
	created := make(map[string][]byte)
	resources := make(map[string][]byte)
	postCount := make(map[string]int)
	var deleted []string
	var replacedResourceVersion string
	jobStatus := map[string]any{"active": float64(1)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			postCount[r.URL.Path]++
			if postCount[r.URL.Path] > 1 {
				http.Error(w, "already exists", http.StatusConflict)
				return
			}
			created[r.URL.Path] = body
			var object struct {
				Metadata kubernetesMetadata `json:"metadata"`
			}
			_ = json.Unmarshal(body, &object)
			var stored map[string]any
			_ = json.Unmarshal(body, &stored)
			stored["metadata"].(map[string]any)["resourceVersion"] = "rv-1"
			resources[r.URL.Path+"/"+object.Metadata.Name], _ = json.Marshal(stored)
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var object struct {
				Metadata kubernetesMetadata `json:"metadata"`
			}
			_ = json.Unmarshal(body, &object)
			replacedResourceVersion = object.Metadata.ResourceVersion
			if replacedResourceVersion == "" {
				http.Error(w, "metadata.resourceVersion is required", http.StatusUnprocessableEntity)
				return
			}
			resources[r.URL.Path] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, ok := resources[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/apis/batch/v1/namespaces/workers/jobs/"+jobName {
				var object map[string]any
				_ = json.Unmarshal(body, &object)
				object["status"] = jobStatus
				body, _ = json.Marshal(object)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			delete(resources, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)

	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, Token: "service-token", HTTPClient: server.Client(),
		Namespace: "workers", Image: "registry.example/flow-worker:v1", ImagePullPolicy: "Always",
		WorkDir: "/workspace", WorkerArgs: []string{"--no-metrics"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := testProfile()
	profile.ProviderOptions["harness_model_proxy_secret_name"] = "flow-harness-model-proxy"
	request := LaunchRequest{
		Identity: identity, Assignment: assignment, Profile: profile,
		CoordinatorURL: "https://coordinator.example", WorkerToken: "private-direct-token",
	}
	if err := provider.Launch(context.Background(), request); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	// Repeating Launch gets 409 for both stable objects and is still successful.
	if err := provider.Launch(context.Background(), request); err != nil {
		t.Fatalf("idempotent Launch() error = %v", err)
	}

	secretPath := "/api/v1/namespaces/workers/secrets"
	jobPath := "/apis/batch/v1/namespaces/workers/jobs"
	if postCount[secretPath] != 2 || postCount[jobPath] != 2 || len(created) != 2 {
		t.Fatalf("POST counts = %v created paths = %v", postCount, created)
	}
	if replacedResourceVersion != "rv-1" {
		t.Fatalf("replacement Secret resource version = %q, want rv-1", replacedResourceVersion)
	}
	var secret kubernetesSecret
	if err := json.Unmarshal(created[secretPath], &secret); err != nil {
		t.Fatal(err)
	}
	if secret.APIVersion != "v1" || secret.Kind != "Secret" || secret.Metadata.Name != secretName || secret.Metadata.Namespace != "workers" {
		t.Fatalf("Secret identity = %+v", secret)
	}
	config := string(secret.Data["worker.yaml"])
	for _, want := range []string{
		"worker_id: worker_1", "coordinator_url: https://coordinator.example",
		"token: private-direct-token", "work_dir: /workspace",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("worker config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, "accepts:") {
		t.Errorf("worker config advertises accepted buckets:\n%s", config)
	}
	if strings.Contains(config, "HARNESS_MODEL_PROXY") {
		t.Errorf("worker assignment Secret must not contain model proxy credentials:\n%s", config)
	}

	var job kubernetesJob
	if err := json.Unmarshal(created[jobPath], &job); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(created[jobPath]), "private-direct-token") {
		t.Fatal("direct token leaked into Job payload")
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" || job.Metadata.Name != jobName || job.Spec.BackoffLimit != 0 {
		t.Fatalf("Job identity/spec = %+v", job)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != "Never" || pod.ServiceAccountName != "default" || len(pod.Containers) != 1 ||
		pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 1000 ||
		pod.SecurityContext.RunAsGroup == nil || *pod.SecurityContext.RunAsGroup != 1000 ||
		pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 1000 {
		t.Fatalf("pod spec = %+v", pod)
	}
	wantCommand := []string{"flow-worker", "run", "--one-shot", "--config", "/var/run/flow/worker.yaml", "--no-metrics"}
	if !reflect.DeepEqual(pod.Containers[0].Command, wantCommand) {
		t.Fatalf("command = %q, want %q", pod.Containers[0].Command, wantCommand)
	}
	container := pod.Containers[0]
	wantEnv := []kubernetesEnvVar{
		{Name: "HARNESS_MODEL_PROXY_URL", ValueFrom: kubernetesEnvVarSource{SecretKeyRef: kubernetesSecretKeySelector{Name: "flow-harness-model-proxy", Key: "HARNESS_MODEL_PROXY_URL"}}},
		{Name: "HARNESS_MODEL_PROXY_API_KEY", ValueFrom: kubernetesEnvVarSource{SecretKeyRef: kubernetesSecretKeySelector{Name: "flow-harness-model-proxy", Key: "HARNESS_MODEL_PROXY_API_KEY"}}},
	}
	if !reflect.DeepEqual(container.Env, wantEnv) {
		t.Fatalf("worker env = %#v, want %#v", container.Env, wantEnv)
	}
	if container.Image != "registry.example/flow-worker:v1" || container.ImagePullPolicy != "Always" ||
		len(container.VolumeMounts) != 2 || container.VolumeMounts[1].MountPath != "/workspace" ||
		len(pod.Volumes) != 2 || pod.Volumes[0].Secret.SecretName != secretName || *pod.Volumes[0].Secret.DefaultMode != 0o400 || pod.Volumes[1].EmptyDir == nil {
		t.Fatalf("container/volume = %+v %+v", container, pod.Volumes)
	}
	if got := job.Metadata.Annotations["flow.clarifiedlabs.com/assignment-id"]; got != assignment.Assignment.ID {
		t.Fatalf("assignment annotation = %q", got)
	}

	status, err := provider.Inspect(context.Background(), identity)
	if err != nil || status != ProviderRunning {
		t.Fatalf("Inspect() = %q, %v", status, err)
	}
	jobStatus = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
	status, err = provider.Inspect(context.Background(), identity)
	if err != nil || status != ProviderSucceeded {
		t.Fatalf("complete Inspect() = %q, %v", status, err)
	}
	if err := provider.Delete(context.Background(), identity); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Model a crash after the private Secret was created but before the Job POST.
	mu.Lock()
	resources[secretPath+"/"+secretName] = append([]byte(nil), created[secretPath]...)
	mu.Unlock()
	status, err = provider.Inspect(context.Background(), identity)
	if err != nil || status != ProviderIncomplete {
		t.Fatalf("secret-only Inspect() = %q, %v, want %q", status, err, ProviderIncomplete)
	}
	if err := provider.Delete(context.Background(), identity); err != nil {
		t.Fatalf("secret-only Delete() error = %v", err)
	}
	wantDeletes := []string{
		"/apis/batch/v1/namespaces/workers/jobs/" + jobName,
		"/api/v1/namespaces/workers/secrets/" + secretName,
		"/api/v1/namespaces/workers/secrets/" + secretName,
	}
	if !reflect.DeepEqual(deleted, wantDeletes) {
		t.Fatalf("deleted paths = %v, want %v", deleted, wantDeletes)
	}
}

func TestHarnessModelProxyEnvironmentRequiresConfiguredSecret(t *testing.T) {
	if got := harnessModelProxyEnvironment(""); got != nil {
		t.Fatalf("empty model proxy Secret env = %#v, want nil", got)
	}
}

func TestKubernetesProviderDeleteTimesOutWhenJobStaysTerminating(t *testing.T) {
	identity := IdentityOf(testAssignment("closed"))
	jobName, _ := KubernetesResourceNames(identity)
	jobPath := "/apis/batch/v1/namespaces/workers/jobs/" + jobName
	labels, annotations := kubernetesIdentityMetadata(identity)
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jobPath {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(kubernetesJob{Metadata: kubernetesMetadata{
				Name: jobName, Namespace: "workers", Labels: labels, Annotations: annotations,
			}})
		case http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, HTTPClient: server.Client(), Namespace: "workers",
		Image: "worker:v1", WorkDir: "/work", DeletionTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Delete(context.Background(), identity)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Delete() error = %v, want bounded deadline", err)
	}
	if deletes != 1 {
		t.Fatalf("Job delete calls = %d, want 1", deletes)
	}
}

func TestKubernetesProviderRejectsForeignResourceConflicts(t *testing.T) {
	assignment := testAssignment("pending")
	identity := IdentityOf(assignment)
	_, secretName := KubernetesResourceNames(identity)
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			http.Error(w, "already exists", http.StatusConflict)
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(kubernetesSecret{
				APIVersion: "v1", Kind: "Secret", Type: "Opaque",
				Metadata: kubernetesMetadata{Name: secretName, Labels: map[string]string{"app.kubernetes.io/managed-by": "somebody-else"}},
			})
		case http.MethodPut, http.MethodDelete:
			writes++
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, HTTPClient: server.Client(), Namespace: "workers", Image: "worker:v1", WorkDir: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Launch(context.Background(), LaunchRequest{
		Identity: identity, Assignment: assignment, Profile: testProfile(),
		CoordinatorURL: "https://coordinator.example", WorkerToken: "worker-token",
	})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("foreign resource conflict error = %v, want permanent", err)
	}
	if writes != 0 {
		t.Fatalf("foreign resource was modified %d times", writes)
	}
}

func TestKubernetesProviderRefusesToDeleteForeignJob(t *testing.T) {
	identity := IdentityOf(testAssignment("pending"))
	jobName, _ := KubernetesResourceNames(identity)
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
		}
		_ = json.NewEncoder(w).Encode(kubernetesJob{Metadata: kubernetesMetadata{Name: jobName}})
	}))
	t.Cleanup(server.Close)
	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, HTTPClient: server.Client(), Namespace: "workers", Image: "worker:v1", WorkDir: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Delete(context.Background(), identity); err == nil || !IsPermanent(err) {
		t.Fatalf("Delete() foreign Job error = %v, want permanent", err)
	}
	if deletes != 0 {
		t.Fatalf("foreign Job was deleted %d times", deletes)
	}
}

func TestKubernetesProviderLeavesSecretForFencedCleanupAfterPermanentlyRejectedJob(t *testing.T) {
	assignment := testAssignment("pending")
	identity := IdentityOf(assignment)
	_, secretName := KubernetesResourceNames(identity)
	var deleted string
	var secretBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/secrets"):
			secretBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			http.Error(w, "invalid job", http.StatusUnprocessableEntity)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+secretName):
			_, _ = w.Write(secretBody)
		case r.Method == http.MethodDelete:
			deleted = r.URL.Path
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, HTTPClient: server.Client(), Namespace: "workers", Image: "worker:v1", WorkDir: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Launch(context.Background(), LaunchRequest{
		Identity: identity, Assignment: assignment, Profile: testProfile(),
		CoordinatorURL: "https://coordinator.example", WorkerToken: "worker-token",
	})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("Launch() error = %v, want permanent rejection", err)
	}
	if deleted != "" {
		t.Fatalf("Secret was deleted before its credential could be fenced: %q", deleted)
	}
}

func TestKubernetesProviderUsesPersistedNamespaceForRecovery(t *testing.T) {
	assignment := testAssignment("pending")
	assignment.Assignment.ProviderOptions["namespace"] = "persisted-workers"
	identity := IdentityOf(assignment)
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write(ownedJobStatusJSON(identity, map[string]any{"active": 1}))
	}))
	t.Cleanup(server.Close)
	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, HTTPClient: server.Client(), Namespace: "new-workers", Image: "worker:v1", WorkDir: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Inspect(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "/namespaces/persisted-workers/jobs/") {
		t.Fatalf("Inspect path = %q, want persisted namespace", path)
	}
}

func TestKubernetesProviderRereadsRotatedBearerToken(t *testing.T) {
	identity := IdentityOf(testAssignment("pending"))
	jobName, _ := KubernetesResourceNames(identity)
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		_, _ = w.Write(ownedJobStatusJSON(identity, map[string]any{"active": 1}))
	}))
	t.Cleanup(server.Close)

	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, TokenFile: tokenFile, HTTPClient: server.Client(),
		Namespace: "workers", Image: "worker:test", WorkDir: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Inspect(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("second-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Inspect(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bearer first-token", "Bearer second-token"}; !reflect.DeepEqual(tokens, want) {
		t.Fatalf("authorization headers for %s = %q, want %q", jobName, tokens, want)
	}
}

func TestKubernetesProviderStatusAndPermanentAPIErrors(t *testing.T) {
	assignment := testAssignment("pending")
	jobName, _ := KubernetesResourceNames(IdentityOf(assignment))
	statusCode := http.StatusNotFound
	response := "not found"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+jobName) {
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(response))
			return
		}
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	provider, err := NewKubernetesProvider(KubernetesProviderOptions{
		BaseURL: server.URL, HTTPClient: server.Client(), Namespace: "workers",
		Image: "worker:v1", WorkDir: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider.Inspect(context.Background(), IdentityOf(assignment))
	if err != nil || got != ProviderNotFound {
		t.Fatalf("not found Inspect() = %q, %v", got, err)
	}
	statusCode = http.StatusUnprocessableEntity
	response = `{"message":"invalid Job"}`
	err = provider.Launch(context.Background(), LaunchRequest{
		Identity: IdentityOf(assignment), Assignment: assignment, Profile: testProfile(),
		CoordinatorURL: "https://coordinator", WorkerToken: "token",
	})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("validation Launch() error = %v, want permanent", err)
	}
}

func ownedJobStatusJSON(identity AssignmentIdentity, status map[string]any) []byte {
	jobName, _ := KubernetesResourceNames(identity)
	labels, annotations := kubernetesIdentityMetadata(identity)
	payload, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": jobName, "labels": labels, "annotations": annotations},
		"status":   status,
	})
	return payload
}

func TestKubernetesResourceNamesAreStableAndDNSSafe(t *testing.T) {
	identity := AssignmentIdentity{
		AssignmentID: "A_Very/Long Assignment.ID With Unsafe Characters And More Characters Than DNS Allows",
		WorkerID:     "worker", ProviderID: "provider", ProfileName: "profile", ProviderRequestID: "request",
	}
	job1, secret1 := KubernetesResourceNames(identity)
	job2, secret2 := KubernetesResourceNames(identity)
	if job1 != job2 || secret1 != secret2 || len(job1) > 63 || len(secret1) > 63 {
		t.Fatalf("unstable/long names: %q %q / %q %q", job1, secret1, job2, secret2)
	}
	for _, name := range []string{job1, secret1} {
		if name != strings.ToLower(name) || strings.ContainsAny(name, "_/. ") {
			t.Fatalf("name is not DNS-safe: %q", name)
		}
	}
}
