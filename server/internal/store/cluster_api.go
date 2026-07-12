package store

// Cluster API access — a credentialed GET against a registered cluster's k8s API,
// used by the native deployment provider to read ArgoCD Application CRs
// server-side. Like ProbeCluster, the decrypted credential never leaves the store:
// the HTTP call lives here, and the caller receives only the response body.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// clusterAPITimeout bounds a single cluster API GET.
const clusterAPITimeout = 10 * time.Second

// maxClusterAPIResponse caps the response body read — an ArgoCD Application can be
// sizeable but never megabytes; the bound stops a hostile/broken endpoint from
// making the control plane read an unbounded stream.
const maxClusterAPIResponse = 4 << 20 // 4 MiB

// ClusterAPIGet issues an authenticated GET to path on a registered cluster's k8s
// API and returns the response body. projectID gates access (allowed_projects) via
// ResolveClusterForDispatch; in_cluster clusters are not reachable from the
// control plane and are rejected. Satisfies deploy.ClusterGetter.
func (s *Store) ClusterAPIGet(ctx context.Context, cluster string, projectID uuid.UUID, path string) ([]byte, error) {
	kubeconfig, inCluster, _, err := s.ResolveClusterForDispatch(ctx, projectID, cluster)
	if err != nil {
		return nil, err // ErrClusterNotFound / not-authorized flow up unchanged
	}
	if inCluster {
		return nil, fmt.Errorf("store: cluster %q is in_cluster — not reachable from the control plane for Application reads", cluster)
	}
	ep, err := parseKubeconfigEndpoint([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("store: cluster %q: %w", cluster, err)
	}
	return doClusterAPIGet(ctx, ep, path)
}

// doClusterAPIGet performs the GET against <ep.server><path>, applying the bearer/
// client-cert credential. 200 → body; anything else → error. Redirects are refused
// (the credential must not be replayed to a 3xx target).
func doClusterAPIGet(ctx context.Context, ep kubeEndpoint, path string) ([]byte, error) {
	server := strings.TrimRight(strings.TrimSpace(ep.server), "/")
	// Defence in depth: re-validate the server here so a legacy/direct-DB row with
	// an http:// or userinfo endpoint can't send a credential anywhere.
	if err := validateHTTPSURL(server, "api_server"); err != nil {
		return nil, err
	}
	client, err := kubeHTTPClient(ep.caPEM, ep.clientCert, ep.insecure, clusterAPITimeout)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, clusterAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, server+path, nil)
	if err != nil {
		return nil, fmt.Errorf("cluster API: build request: %w", err)
	}
	if ep.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+ep.bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL is safe to surface (the token is a header, not in the URL).
		return nil, fmt.Errorf("cluster API GET: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxClusterAPIResponse))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cluster API GET %s: unexpected status %d", path, resp.StatusCode)
	}
	if readErr != nil {
		return nil, fmt.Errorf("cluster API GET %s: read body: %w", path, readErr)
	}
	return body, nil
}

// kubeHTTPClient builds an HTTPS client for a k8s API endpoint: TLS from the CA
// (or insecure per an explicit kubeconfig opt-in), optional client cert, and a
// redirect refusal so a credentialed request is never replayed to a 3xx target.
// Shared by the connectivity probe and the API-get path.
func kubeHTTPClient(caPEM []byte, clientCert *tls.Certificate, insecure bool, timeout time.Duration) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case insecure:
		tlsCfg.InsecureSkipVerify = true // explicit per the kubeconfig; the operator chose it
	case len(caPEM) > 0:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("CA certificate is not valid PEM")
		}
		tlsCfg.RootCAs = pool
	}
	if clientCert != nil {
		tlsCfg.Certificates = []tls.Certificate{*clientCert}
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{TLSClientConfig: tlsCfg},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}
