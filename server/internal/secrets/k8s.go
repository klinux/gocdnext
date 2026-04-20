package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// KubernetesResolver reads project secrets from K8s Secret objects.
// Naming: one Secret per project, named `gocdnext-secrets-{slug}`
// (override via NameTemplate). Each key in the Secret is a secret
// name; value is the plaintext used as env var value at dispatch
// time.
//
// Projects map via UUID → slug (one store lookup, cached per
// request) so the scheduler calls resolver.Resolve(projectID, names)
// exactly the same way it does for DBResolver.
//
// Example Secret:
//
//	apiVersion: v1
//	kind: Secret
//	metadata:
//	  name: gocdnext-secrets-my-project
//	  namespace: gocdnext
//	stringData:
//	  GH_TOKEN: ghp_abc
//	  DEPLOY_KEY: "..."
type KubernetesResolver struct {
	store     *store.Store
	client    kubernetes.Interface
	namespace string
	template  string // e.g. "gocdnext-secrets-{slug}"
}

// KubernetesResolverConfig configures the resolver.
type KubernetesResolverConfig struct {
	Store          *store.Store
	Client         kubernetes.Interface // optional for tests
	Namespace      string
	KubeconfigPath string // empty = in-cluster
	// NameTemplate controls the Secret-name layout. {slug} expands
	// to the project's slug. Default: "gocdnext-secrets-{slug}".
	NameTemplate string
}

// NewKubernetesResolver returns a ready resolver. If Client is nil,
// builds one from KubeconfigPath (empty = in-cluster).
func NewKubernetesResolver(cfg KubernetesResolverConfig) (*KubernetesResolver, error) {
	if cfg.Store == nil {
		return nil, errors.New("secrets: KubernetesResolver needs a store")
	}
	if cfg.Namespace == "" {
		return nil, errors.New("secrets: KubernetesResolver needs a namespace")
	}
	client := cfg.Client
	if client == nil {
		restCfg, err := loadRESTConfig(cfg.KubeconfigPath)
		if err != nil {
			return nil, err
		}
		client, err = kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, fmt.Errorf("secrets: kubernetes: build client: %w", err)
		}
	}
	template := cfg.NameTemplate
	if template == "" {
		template = "gocdnext-secrets-{slug}"
	}
	return &KubernetesResolver{
		store:     cfg.Store,
		client:    client,
		namespace: cfg.Namespace,
		template:  template,
	}, nil
}

// Resolve satisfies the Resolver contract. Missing Secret (no row
// for this project) OR missing keys are silently omitted so the
// scheduler can produce a precise "secrets not set" diff. Only
// outright errors (network, RBAC) escape as err.
func (r *KubernetesResolver) Resolve(ctx context.Context, projectID uuid.UUID, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	slug, err := r.projectSlug(ctx, projectID)
	if err != nil {
		return nil, err
	}
	secretName := strings.ReplaceAll(r.template, "{slug}", slug)

	sec, err := r.client.CoreV1().Secrets(r.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		// No Secret yet — treat every name as "unset". Scheduler's
		// diff will complain about the specific ones.
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: kubernetes: get %q: %w", secretName, err)
	}

	out := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := sec.Data[n]; ok {
			out[n] = string(v)
			continue
		}
		// stringData lands in Data after Kubernetes merges it; if
		// someone PATCHes with stringData we'll still find the key
		// under Data, so no fallback needed.
	}
	return out, nil
}

func (r *KubernetesResolver) projectSlug(ctx context.Context, projectID uuid.UUID) (string, error) {
	proj, err := r.store.GetProjectByID(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("secrets: kubernetes: project %s: %w", projectID, err)
	}
	return proj.Slug, nil
}

// loadRESTConfig picks in-cluster when kubeconfig is empty, else
// reads the kubeconfig path.
func loadRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("secrets: kubernetes: kubeconfig %s: %w", kubeconfigPath, err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("secrets: kubernetes: in-cluster config: %w", err)
	}
	return cfg, nil
}
