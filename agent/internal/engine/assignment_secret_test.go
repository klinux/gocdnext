package engine

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildAssignmentSecret_HappyPath(t *testing.T) {
	bytesIn := []byte("\x08\x01\x10\x02") // fake proto bytes
	s, err := BuildAssignmentSecret("gocdnext-job-abc-assignment", "ci", bytesIn, "run-1", "job-1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if s.Type != corev1.SecretTypeOpaque {
		t.Errorf("type: got %v", s.Type)
	}
	if !bytes.Equal(s.Data[AssignmentSecretKey], bytesIn) {
		t.Errorf("data round-trip mismatch")
	}
	if s.Labels["gocdnext.io/run-id"] != "run-1" {
		t.Errorf("run-id label: got %q", s.Labels["gocdnext.io/run-id"])
	}
	if s.Labels["gocdnext.io/job-id"] != "job-1" {
		t.Errorf("job-id label: got %q", s.Labels["gocdnext.io/job-id"])
	}
}

func TestBuildAssignmentSecret_RejectsOverflow(t *testing.T) {
	big := make([]byte, AssignmentSecretMaxBytes+1)
	if _, err := BuildAssignmentSecret("n", "ci", big, "r", "j"); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestBuildAssignmentSecret_RejectsEmpty(t *testing.T) {
	if _, err := BuildAssignmentSecret("n", "ci", nil, "r", "j"); err == nil {
		t.Fatal("expected empty payload error")
	}
	if _, err := BuildAssignmentSecret("", "ci", []byte("x"), "r", "j"); err == nil {
		t.Fatal("expected empty name error")
	}
	if _, err := BuildAssignmentSecret("n", "", []byte("x"), "r", "j"); err == nil {
		t.Fatal("expected empty namespace error")
	}
}

func TestPatchAssignmentSecretOwner_AppliesOwnerRef(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s1",
			Namespace: "ci",
		},
	})
	k := NewKubernetesWithClient(client, KubernetesConfig{Namespace: "ci"})

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: types.UID("uid-xyz"), Namespace: "ci"}}
	if err := k.PatchAssignmentSecretOwner(context.Background(), "s1", pod); err != nil {
		t.Fatalf("patch: %v", err)
	}

	got, err := client.CoreV1().Secrets("ci").Get(context.Background(), "s1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n := len(got.OwnerReferences); n != 1 {
		t.Fatalf("owner refs: want 1, got %d", n)
	}
	or := got.OwnerReferences[0]
	if or.Name != "p1" {
		t.Errorf("owner name: got %q", or.Name)
	}
	if or.UID != "uid-xyz" {
		t.Errorf("owner uid: got %q", or.UID)
	}
	if or.Kind != "Pod" {
		t.Errorf("owner kind: got %q", or.Kind)
	}
}
