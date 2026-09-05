package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const selfSubjectAccessReviewPath = "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"

// ClusterAccessCheck is one Kubernetes RBAC permission gocdnext wants to verify
// against a registered cluster credential. It maps directly to
// SelfSubjectAccessReview.spec.resourceAttributes.
type ClusterAccessCheck struct {
	Group       string
	Resource    string
	Subresource string
	Verb        string
	Namespace   string
	Name        string
}

// ClusterAccessDeniedError is returned only when Kubernetes itself answered the
// SelfSubjectAccessReview with allowed=false. The message is safe to show to an
// operator: it names the missing verb/resource, never the credential.
type ClusterAccessDeniedError struct {
	Cluster string
	Check   ClusterAccessCheck
	Reason  string
}

func (e *ClusterAccessDeniedError) Error() string {
	msg := fmt.Sprintf("cluster credential for %q is missing Kubernetes RBAC: cannot %s",
		e.Cluster, e.Check.describe())
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg
}

// ClusterAccessReviewUnavailableError means the review endpoint could not produce
// an authoritative allow/deny answer. Callers that use access reviews as a
// diagnostic preflight may safely fall back to the real operation.
type ClusterAccessReviewUnavailableError struct {
	Cluster string
	Check   ClusterAccessCheck
	Err     string
}

func (e *ClusterAccessReviewUnavailableError) Error() string {
	msg := fmt.Sprintf("cluster RBAC review for %q could not verify %s", e.Cluster, e.Check.describe())
	if e.Err != "" {
		msg += ": " + e.Err
	}
	return msg
}

func (c ClusterAccessCheck) describe() string {
	resource := c.Resource
	if c.Subresource != "" {
		resource += "/" + c.Subresource
	}
	if c.Group != "" {
		resource += "." + c.Group
	}

	parts := []string{strings.TrimSpace(c.Verb), resource}
	if c.Name != "" {
		parts = append(parts, fmt.Sprintf("named %q", c.Name))
	}
	if c.Namespace != "" {
		parts = append(parts, fmt.Sprintf("in namespace %q", c.Namespace))
	}
	return strings.Join(parts, " ")
}

type selfSubjectAccessReviewResponse struct {
	Status struct {
		Allowed         bool   `json:"allowed"`
		Denied          bool   `json:"denied"`
		Reason          string `json:"reason"`
		EvaluationError string `json:"evaluationError"`
	} `json:"status"`
}

// CheckClusterAccess verifies each check with Kubernetes SelfSubjectAccessReview.
// It never mutates cluster RBAC. in_cluster credentials are skipped because the
// control plane cannot impersonate the agent pod's ServiceAccount; the runtime
// operation remains the source of truth for that mode.
func (s *Store) CheckClusterAccess(ctx context.Context, projectID uuid.UUID, cluster string, checks []ClusterAccessCheck) error {
	if len(checks) == 0 {
		return nil
	}
	kubeconfig, inCluster, _, err := s.ResolveClusterForDispatch(ctx, projectID, cluster)
	if err != nil {
		return err
	}
	if inCluster {
		return nil
	}
	ep, err := parseKubeconfigEndpoint([]byte(kubeconfig))
	if err != nil {
		return fmt.Errorf("store: cluster %q: %w", cluster, err)
	}

	seen := make(map[ClusterAccessCheck]struct{}, len(checks))
	for _, check := range checks {
		check = normalizeAccessCheck(check)
		if check.Verb == "" || check.Resource == "" {
			return fmt.Errorf("store: cluster access review: verb and resource are required")
		}
		if _, ok := seen[check]; ok {
			continue
		}
		seen[check] = struct{}{}

		status, err := doClusterAPIAccessReview(ctx, ep, check)
		if err != nil {
			return err
		}
		evalErr := reviewText(status.Status.EvaluationError)
		if evalErr != "" && !status.Status.Allowed && !status.Status.Denied {
			return &ClusterAccessReviewUnavailableError{Cluster: cluster, Check: check, Err: evalErr}
		}
		if !status.Status.Allowed {
			reason := reviewText(status.Status.Reason)
			if reason == "" {
				reason = evalErr
			}
			return &ClusterAccessDeniedError{Cluster: cluster, Check: check, Reason: reason}
		}
	}
	return nil
}

func doClusterAPIAccessReview(ctx context.Context, ep kubeEndpoint, check ClusterAccessCheck) (selfSubjectAccessReviewResponse, error) {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{
			"resourceAttributes": map[string]string{
				"group":       check.Group,
				"resource":    check.Resource,
				"subresource": check.Subresource,
				"verb":        check.Verb,
				"namespace":   check.Namespace,
				"name":        check.Name,
			},
		},
	})
	if err != nil {
		return selfSubjectAccessReviewResponse{}, fmt.Errorf("cluster API selfsubjectaccessreview: encode: %w", err)
	}
	raw, err := doClusterAPIWrite(ctx, ep, "POST", "application/json", selfSubjectAccessReviewPath, body)
	if err != nil {
		return selfSubjectAccessReviewResponse{}, err
	}
	var res selfSubjectAccessReviewResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return selfSubjectAccessReviewResponse{}, fmt.Errorf("cluster API selfsubjectaccessreview: decode response: %w", err)
	}
	return res, nil
}

func normalizeAccessCheck(c ClusterAccessCheck) ClusterAccessCheck {
	c.Group = strings.TrimSpace(c.Group)
	c.Resource = strings.TrimSpace(c.Resource)
	c.Subresource = strings.TrimSpace(c.Subresource)
	c.Verb = strings.TrimSpace(c.Verb)
	c.Namespace = strings.TrimSpace(c.Namespace)
	c.Name = strings.TrimSpace(c.Name)
	return c
}

func reviewText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxRunes = 240
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
}
