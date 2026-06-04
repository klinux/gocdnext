package engine

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// AssignmentSecretKey is the data key inside the Secret where the
// init container reads the serialised JobAssignment.
const AssignmentSecretKey = "assignment.pb"

// AssignmentSecretMaxBytes caps the serialised assignment to leave
// margin under k8s's 1 MiB Secret limit. JobAssignment carries
// scripts, env, signed URLs — typically a few KiB; 950 KiB is more
// than any honest job needs, and a hard fail here surfaces a
// runaway YAML before kubelet rejects the Secret.
const AssignmentSecretMaxBytes = 950 * 1024

// BuildAssignmentSecret materialises the Secret that the "prep"
// init container will mount in isolated mode. The caller (runner)
// serialises the proto and passes the bytes — keeping the engine
// package proto-free.
//
// labels carries (run_id, job_id) for orphan-cleanup label
// selectors. Both are accepted as plain strings and not
// sanitised here; the caller is responsible for DNS-1123
// conformance.
func BuildAssignmentSecret(name, namespace string, assignmentBytes []byte, runID, jobID string) (*corev1.Secret, error) {
	if name == "" {
		return nil, fmt.Errorf("assignment secret: empty name")
	}
	if namespace == "" {
		return nil, fmt.Errorf("assignment secret: empty namespace")
	}
	if len(assignmentBytes) == 0 {
		return nil, fmt.Errorf("assignment secret: empty payload")
	}
	if len(assignmentBytes) > AssignmentSecretMaxBytes {
		return nil, fmt.Errorf("assignment secret: payload %d bytes exceeds max %d",
			len(assignmentBytes), AssignmentSecretMaxBytes)
	}
	labels := map[string]string{
		"app.kubernetes.io/name":       "gocdnext-job-assignment",
		"app.kubernetes.io/managed-by": "gocdnext-agent",
	}
	if v, ok := sanitizeLabelValue(runID); ok {
		labels["gocdnext.io/run-id"] = v
	}
	if v, ok := sanitizeLabelValue(jobID); ok {
		labels["gocdnext.io/job-id"] = v
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{AssignmentSecretKey: assignmentBytes},
	}, nil
}

// PatchAssignmentSecretOwner applies an OwnerReference to the
// secret so the kubelet's garbage collector deletes it when the
// pod disappears (job complete, cancel, agent crash → pod GC).
//
// Strategic merge patch keeps the existing labels intact while
// adding the ownerReferences field. Idempotent — calling twice
// is a successful no-op (kubectl re-applies the same set).
func (k *Kubernetes) PatchAssignmentSecretOwner(ctx context.Context, secretName string, pod *corev1.Pod) error {
	if pod == nil || pod.UID == "" {
		return fmt.Errorf("patch owner: pod must have a UID (was Create returned?)")
	}
	isController := true
	blockDel := true
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"ownerReferences": []metav1.OwnerReference{{
				APIVersion:         "v1",
				Kind:               "Pod",
				Name:               pod.Name,
				UID:                pod.UID,
				Controller:         &isController,
				BlockOwnerDeletion: &blockDel,
			}},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("patch owner: marshal: %w", err)
	}
	_, err = k.client.CoreV1().Secrets(k.cfg.Namespace).Patch(
		ctx, secretName, types.StrategicMergePatchType, body, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch owner: %w", err)
	}
	return nil
}
