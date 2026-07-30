package orchestrator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// In-cluster service-account locations for the Kubernetes API client.
const (
	serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	serviceAccountCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// K8sScaler scales one Kubernetes Deployment via the apps/v1
// deployments/scale subresource, using only the standard library.
type K8sScaler struct {
	baseURL    string
	token      string
	namespace  string
	deployment string
	client     *http.Client
}

// NewK8sScaler builds a scaler against an arbitrary API endpoint (used by
// tests, and by callers that manage TLS themselves). A nil client uses a
// default client with a 10s timeout.
func NewK8sScaler(baseURL string, token string, client *http.Client, namespace string, deployment string) *K8sScaler {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &K8sScaler{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:      token,
		namespace:  namespace,
		deployment: deployment,
		client:     client,
	}
}

// NewInClusterK8sScaler builds a scaler from the pod's service account:
// the token and CA bundle mounted by Kubernetes, and the API address from
// KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT.
func NewInClusterK8sScaler(namespace string, deployment string) (*K8sScaler, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if host == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is not set: flow-orchestrator must run in a Kubernetes cluster")
	}
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if port == "" {
		port = "443"
	}

	tokenBytes, err := os.ReadFile(serviceAccountTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, errors.New("service account token is empty")
	}

	caPEM, err := os.ReadFile(serviceAccountCAPath)
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse service account CA %s: no certificates", serviceAccountCAPath)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
	}

	return NewK8sScaler("https://"+host+":"+port, token, client, namespace, deployment), nil
}

// scalePath is the deployments/scale subresource URL for the target.
func (k *K8sScaler) scalePath() string {
	return fmt.Sprintf("%s/apis/apps/v1/namespaces/%s/deployments/%s/scale",
		k.baseURL, k.namespace, k.deployment)
}

// scaleBody mirrors the parts of the apps/v1 Scale object we read and write.
type scaleBody struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Metadata   struct {
		Name      string `json:"name,omitempty"`
		Namespace string `json:"namespace,omitempty"`
	} `json:"metadata,omitempty"`
	Spec struct {
		Replicas int `json:"replicas"`
	} `json:"spec"`
}

// GetScale returns the Deployment's spec replica count.
func (k *K8sScaler) GetScale(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.scalePath(), nil)
	if err != nil {
		return 0, fmt.Errorf("build scale request: %w", err)
	}
	k.authorize(req)
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("get scale: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("get scale: unexpected status %s", resp.Status)
	}
	var body scaleBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode scale: %w", err)
	}

	return body.Spec.Replicas, nil
}

// SetScale replaces the Deployment's spec replica count.
func (k *K8sScaler) SetScale(ctx context.Context, replicas int) error {
	body := scaleBody{APIVersion: "apps/v1", Kind: "Scale"}
	body.Metadata.Name = k.deployment
	body.Metadata.Namespace = k.namespace
	body.Spec.Replicas = replicas
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode scale: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, k.scalePath(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build scale request: %w", err)
	}
	k.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("set scale: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("set scale: unexpected status %s", resp.Status)
	}

	return nil
}

func (k *K8sScaler) authorize(req *http.Request) {
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
}
