package engine

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// PodExecutor runs a command inside a specific container of a
// running pod and streams stdio between the caller and that
// in-pod process. Used by the Kubernetes engine in isolated
// workspace mode to drive post-task work (tar + signed-URL PUT)
// from inside the "housekeeper" sidecar without round-tripping
// through the agent's local filesystem.
//
// Implementations should return nil on a clean exit-0 from the
// in-pod command, an error containing the non-zero exit code (or
// the wrapper "command terminated"), or a transport error for
// stream failures. The interface stays narrow so tests can supply
// an in-memory fake — the real SPDY round-trip needs a kubelet at
// the other end of the websocket which `fake.Clientset` can't
// model.
type PodExecutor interface {
	Exec(ctx context.Context, pod, container string, cmd []string,
		stdin io.Reader, stdout, stderr io.Writer) error
}

// ExecOptions describes a request to PodExecutor.Exec.
// Kept as a struct (not positional args) so future fields (TTY,
// resize signals) don't break callers.
type ExecOptions struct {
	Pod       string
	Container string
	Command   []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

// spdyExecutor implements PodExecutor over the SPDY remotecommand
// API — the same wire protocol kubectl exec uses. Requires
// `pods/exec` RBAC on the agent's ServiceAccount.
type spdyExecutor struct {
	client    kubernetes.Interface
	restCfg   *rest.Config
	namespace string
}

// NewSPDYExecutor returns a PodExecutor that talks to the API
// server via SPDY. namespace must be the engine's configured
// namespace; pods outside it are not routable (POST exec endpoint
// is namespace-scoped).
func NewSPDYExecutor(client kubernetes.Interface, restCfg *rest.Config, namespace string) PodExecutor {
	return &spdyExecutor{client: client, restCfg: restCfg, namespace: namespace}
}

func (s *spdyExecutor) Exec(ctx context.Context, pod, container string, cmd []string,
	stdin io.Reader, stdout, stderr io.Writer) error {
	if s == nil {
		return errors.New("exec: nil executor")
	}
	if pod == "" || container == "" {
		return fmt.Errorf("exec: pod=%q container=%q both required", pod, container)
	}
	if len(cmd) == 0 {
		return errors.New("exec: empty command")
	}

	req := s.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(s.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("exec: build SPDY executor: %w", err)
	}
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("exec %s/%s %v: %w", pod, container, cmd, err)
	}
	return nil
}
