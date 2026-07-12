package store

// Cluster connectivity probe — a control-plane "test connection" for a
// registered deploy target. It resolves the stored credential and issues
// a GET <api_server>/version, reporting reachability + TLS + whether the
// credential is accepted. The credential never leaves the store (the
// HTTP call lives here so the decrypted token isn't handed to a caller),
// and never appears in the result message.
//
// SSRF note: this makes the control plane issue an HTTPS GET to an
// operator-supplied URL. It is gated to admins (who already register the
// URL the agent connects to for real deploys), so it grants no
// capability beyond what an admin has — and a private-IP block is
// deliberately NOT applied, since Kubernetes API servers are routinely
// on private addresses.

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// ClusterProbeResult is the outcome of a connectivity test. Status is a
// stable enum the UI maps to a colour; Message is sanitized.
type ClusterProbeResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Probe statuses.
const (
	ClusterProbeOK           = "ok"
	ClusterProbeUnauthorized = "unauthorized"
	ClusterProbeUnreachable  = "unreachable"
	ClusterProbeSkipped      = "skipped"
	ClusterProbeError        = "error"
)

// clusterProbeTimeout bounds the whole probe so the admin button can't
// hang on an unreachable endpoint.
const clusterProbeTimeout = 8 * time.Second

// ProbeCluster connectivity-checks a registered cluster by id.
// in_cluster can't be reached from the control plane (it's the agent
// pod's ServiceAccount) → returns skipped.
func (s *Store) ProbeCluster(ctx context.Context, id uuid.UUID) (ClusterProbeResult, error) {
	c, err := s.GetCluster(ctx, id)
	if err != nil {
		return ClusterProbeResult{}, err // ErrClusterNotFound flows up
	}
	if c.AuthType == ClusterAuthInCluster {
		return ClusterProbeResult{
			Status:  ClusterProbeSkipped,
			Message: "in_cluster targets use the agent pod's ServiceAccount — verified at job runtime, not reachable from the control plane",
		}, nil
	}

	enc, err := s.q.GetClusterCredentialEnc(ctx, pgUUID(id))
	if err != nil {
		return ClusterProbeResult{}, fmt.Errorf("store: probe cluster %s: %w", id, err)
	}
	dec, err := s.decryptCredential(enc, c.Name)
	if err != nil {
		return ClusterProbeResult{}, err
	}

	var (
		server     string
		caPEM      []byte
		bearer     string
		clientCert *tls.Certificate
		insecure   bool
	)
	switch c.AuthType {
	case ClusterAuthToken:
		server, caPEM, bearer = c.APIServer, c.CACert, string(dec)
	case ClusterAuthKubeconfig:
		ep, perr := parseKubeconfigEndpoint(dec)
		if perr != nil {
			// A kubeconfig we can't turn into a probe (exec auth, file-path
			// certs, malformed) is a clear, non-fatal result, not a 500.
			return ClusterProbeResult{Status: ClusterProbeError, Message: perr.Error()}, nil
		}
		server, caPEM, bearer, clientCert, insecure = ep.server, ep.caPEM, ep.bearer, ep.clientCert, ep.insecure
	default:
		return ClusterProbeResult{}, fmt.Errorf("store: probe cluster %q: unknown auth_type %q", c.Name, c.AuthType)
	}

	return probeKubeAPI(ctx, server, caPEM, bearer, clientCert, insecure), nil
}

type kubeEndpoint struct {
	server     string
	caPEM      []byte
	bearer     string
	clientCert *tls.Certificate
	insecure   bool
}

// parseKubeconfigEndpoint extracts the connection details for the
// current context: server + CA + (bearer token OR client cert). It is
// strict — exec auth, file-path certs/CA/token, an unresolved
// current-context, an http/userinfo server, or no supported credential
// all fail with a clear message rather than producing a misleading
// probe result.
func parseKubeconfigEndpoint(kc []byte) (kubeEndpoint, error) {
	var doc struct {
		CurrentContext string `yaml:"current-context"`
		Clusters       []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server   string `yaml:"server"`
				CAData   string `yaml:"certificate-authority-data"`
				CAFile   string `yaml:"certificate-authority"`
				Insecure bool   `yaml:"insecure-skip-tls-verify"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			Name string `yaml:"name"`
			User struct {
				Token          string `yaml:"token"`
				TokenFile      string `yaml:"tokenFile"`
				ClientCertData string `yaml:"client-certificate-data"`
				ClientCertFile string `yaml:"client-certificate"`
				ClientKeyData  string `yaml:"client-key-data"`
				ClientKeyFile  string `yaml:"client-key"`
				Exec           any    `yaml:"exec"`
			} `yaml:"user"`
		} `yaml:"users"`
		Contexts []struct {
			Name    string `yaml:"name"`
			Context struct {
				Cluster string `yaml:"cluster"`
				User    string `yaml:"user"`
			} `yaml:"context"`
		} `yaml:"contexts"`
	}
	if err := yaml.Unmarshal(kc, &doc); err != nil {
		return kubeEndpoint{}, fmt.Errorf("kubeconfig parse failed: %v", err)
	}

	// Resolve the current context strictly — never fall back to "the
	// first cluster/user", which could probe the wrong target.
	if strings.TrimSpace(doc.CurrentContext) == "" {
		return kubeEndpoint{}, errors.New("kubeconfig has no current-context")
	}
	var clusterName, userName string
	ctxFound := false
	for _, c := range doc.Contexts {
		if c.Name == doc.CurrentContext {
			clusterName, userName, ctxFound = c.Context.Cluster, c.Context.User, true
			break
		}
	}
	if !ctxFound {
		return kubeEndpoint{}, fmt.Errorf("kubeconfig current-context %q has no matching context", doc.CurrentContext)
	}

	var server, caData, caFile string
	var insecure, clFound bool
	for _, c := range doc.Clusters {
		if c.Name == clusterName {
			server, caData, caFile, insecure, clFound = c.Cluster.Server, c.Cluster.CAData, c.Cluster.CAFile, c.Cluster.Insecure, true
			break
		}
	}
	if !clFound {
		return kubeEndpoint{}, fmt.Errorf("kubeconfig context references unknown cluster %q", clusterName)
	}
	if caFile != "" {
		return kubeEndpoint{}, errors.New("kubeconfig uses a file-path CA (certificate-authority); only certificate-authority-data is testable from the control plane")
	}
	if err := validateHTTPSURL(server, "kubeconfig server"); err != nil {
		return kubeEndpoint{}, err
	}

	var u struct {
		Token, TokenFile, CertData, CertFile, KeyData, KeyFile string
		Exec                                                   any
	}
	usrFound := false
	for _, x := range doc.Users {
		if x.Name == userName {
			u.Token, u.TokenFile = x.User.Token, x.User.TokenFile
			u.CertData, u.CertFile = x.User.ClientCertData, x.User.ClientCertFile
			u.KeyData, u.KeyFile = x.User.ClientKeyData, x.User.ClientKeyFile
			u.Exec, usrFound = x.User.Exec, true
			break
		}
	}
	if !usrFound {
		return kubeEndpoint{}, fmt.Errorf("kubeconfig context references unknown user %q", userName)
	}
	if u.Exec != nil {
		return kubeEndpoint{}, errors.New("kubeconfig uses exec auth, which can't be tested from the control plane")
	}
	if u.TokenFile != "" || u.CertFile != "" || u.KeyFile != "" {
		return kubeEndpoint{}, errors.New("kubeconfig uses file-path credentials (tokenFile / client-certificate / client-key); only the inline *-data forms are testable from the control plane")
	}

	ep := kubeEndpoint{server: server, bearer: u.Token, insecure: insecure}
	if caData != "" {
		raw, derr := base64.StdEncoding.DecodeString(caData)
		if derr != nil {
			return kubeEndpoint{}, errors.New("kubeconfig certificate-authority-data is not valid base64")
		}
		ep.caPEM = raw
	}
	if u.CertData != "" && u.KeyData != "" {
		cpem, e1 := base64.StdEncoding.DecodeString(u.CertData)
		kpem, e2 := base64.StdEncoding.DecodeString(u.KeyData)
		if e1 != nil || e2 != nil {
			return kubeEndpoint{}, errors.New("kubeconfig client cert/key data is not valid base64")
		}
		pair, e3 := tls.X509KeyPair(cpem, kpem)
		if e3 != nil {
			return kubeEndpoint{}, fmt.Errorf("kubeconfig client certificate: %v", e3)
		}
		ep.clientCert = &pair
	}
	// A probe with no credential can't distinguish "auth works" from
	// "anonymous access" — refuse rather than risk a false ok via
	// anonymous RBAC.
	if ep.bearer == "" && ep.clientCert == nil {
		return kubeEndpoint{}, errors.New("kubeconfig has no supported credential to test (token or client-certificate-data + client-key-data)")
	}
	return ep, nil
}

func probeKubeAPI(ctx context.Context, server string, caPEM []byte, bearer string, clientCert *tls.Certificate, insecure bool) ClusterProbeResult {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	// Defence in depth: re-validate the server here (not only at
	// registration) so a legacy / direct-DB row with an http:// or
	// userinfo api_server can't send a credential anywhere before this
	// point. Idempotent for the kubeconfig path (already validated).
	if err := validateHTTPSURL(server, "api_server"); err != nil {
		return ClusterProbeResult{Status: ClusterProbeError, Message: err.Error()}
	}

	// Shared client build (TLS from CA / insecure, optional client cert, redirect
	// refusal so a credentialed probe never replays to a 3xx target).
	client, cerr := kubeHTTPClient(caPEM, clientCert, insecure, clusterProbeTimeout)
	if cerr != nil {
		return ClusterProbeResult{Status: ClusterProbeError, Message: cerr.Error()}
	}

	cctx, cancel := context.WithTimeout(ctx, clusterProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, server+"/version", nil)
	if err != nil {
		return ClusterProbeResult{Status: ClusterProbeError, Message: "invalid API server URL"}
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		// The transport error carries the URL (not secret — the token is
		// a header), so it's safe to surface for diagnosis.
		return ClusterProbeResult{Status: ClusterProbeUnreachable, Message: "could not reach the API server: " + err.Error()}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return ClusterProbeResult{Status: ClusterProbeOK, Message: "connected — " + kubeVersion(resp)}
	case resp.StatusCode == http.StatusUnauthorized:
		return ClusterProbeResult{Status: ClusterProbeUnauthorized, Message: "credential rejected (401 Unauthorized)"}
	case resp.StatusCode == http.StatusForbidden:
		return ClusterProbeResult{Status: ClusterProbeOK, Message: "credential accepted (403 — connection OK, RBAC is limited)"}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return ClusterProbeResult{Status: ClusterProbeError, Message: fmt.Sprintf("API server returned a redirect (%d) — refusing to follow it with a credential attached", resp.StatusCode)}
	default:
		return ClusterProbeResult{Status: ClusterProbeError, Message: fmt.Sprintf("unexpected status %d from /version", resp.StatusCode)}
	}
}

// kubeVersion best-effort reads the gitVersion from a /version response.
func kubeVersion(resp *http.Response) string {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v) == nil && v.GitVersion != "" {
		return "Kubernetes " + v.GitVersion
	}
	return "Kubernetes API reachable"
}
