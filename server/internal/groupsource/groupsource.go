// Package groupsource resolves approver group membership from an external platform
// over HTTP, at the moment of a vote (ADR-0002 decision 4). It implements
// store.ApproverGroupSource; the store calls it outside the decision transaction and
// fails the vote closed (503) on any error this package returns.
//
// Contract (one request per vote that needs it, no caching):
//
//	GET <url>?provider=<users.provider>&subject=<users.external_id>
//	Authorization: Bearer <token>
//
//	200 {"groups": ["sre", "platform"]}  — the user's current groups
//	404                                  — user unknown to the platform: no groups
//	anything else                        — error (vote refused, retryable)
package groupsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const (
	// DefaultTimeout bounds each lookup. A vote that needs the platform waits at most
	// this long before it is refused with 503.
	DefaultTimeout = 2 * time.Second

	maxBodyBytes    = 64 << 10
	maxGroups       = 1024
	maxGroupNameLen = 255
)

// ErrCircuitOpen is returned without a round trip while the platform is considered
// down. Like every error here, the store turns it into a refused (503) vote.
var ErrCircuitOpen = errors.New("groupsource: circuit open after repeated failures")

// Options configures an HTTP source.
type Options struct {
	// URL is the absolute endpoint. https, or http to a loopback host only: the
	// request carries a bearer token.
	URL string
	// Token is sent as `Authorization: Bearer`. Required.
	Token string
	// Timeout bounds each lookup; 0 means DefaultTimeout.
	Timeout time.Duration
	// Client overrides the HTTP client (tests). Redirects are refused either way.
	Client *http.Client
}

// HTTP is an ApproverGroupSource backed by the platform's HTTP endpoint.
type HTTP struct {
	endpoint *url.URL
	token    string
	timeout  time.Duration
	client   *http.Client
	breaker  *breaker
}

var _ store.ApproverGroupSource = (*HTTP)(nil)

// New validates o and returns a source. Errors name the offending option, never the
// token.
func New(o Options) (*HTTP, error) {
	u, err := url.Parse(o.URL)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return nil, errors.New("groupsource: url must be an absolute http(s) URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return nil, errors.New("groupsource: url must use https (plain http only to a loopback host)")
	}
	if u.User != nil {
		return nil, errors.New("groupsource: url must not embed credentials; use the token")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("groupsource: url must not carry a query or fragment")
	}
	if o.Token == "" {
		return nil, errors.New("groupsource: token is required")
	}
	if o.Timeout < 0 {
		return nil, errors.New("groupsource: timeout must not be negative")
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	client := &http.Client{}
	if o.Client != nil {
		c := *o.Client
		client = &c
	}
	// A redirect would replay the bearer token to wherever it points.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &HTTP{endpoint: u, token: o.Token, timeout: timeout, client: client, breaker: newBreaker()}, nil
}

// Host is the endpoint's host, for the startup log (never the token).
func (h *HTTP) Host() string { return h.endpoint.Host }

// ApproverGroupNames returns who's current groups on the platform. An identity with no
// external subject (local user, service account) has none to resolve: nil, nil
// without a call.
func (h *HTTP) ApproverGroupNames(ctx context.Context, who store.ApproverIdentity) ([]string, error) {
	if who.Provider == "" || who.Subject == "" ||
		who.Provider == store.ProviderLocal || who.Provider == store.ProviderServiceAccount {
		return nil, nil
	}
	if !h.breaker.allow() {
		metrics.ApproverGroupLookup.WithLabelValues("circuit_open").Observe(0)
		return nil, ErrCircuitOpen
	}

	start := time.Now()
	groups, outcome, err := h.lookup(ctx, who)
	metrics.ApproverGroupLookup.WithLabelValues(outcome).Observe(time.Since(start).Seconds())

	switch {
	case err == nil:
		h.breaker.record(healthy)
	case ctx.Err() != nil:
		// The caller went away; that says nothing about the platform.
		h.breaker.record(neutral)
	default:
		h.breaker.record(unhealthy)
	}
	return groups, err
}

// lookup does one round trip and returns the groups and the metric outcome.
func (h *HTTP) lookup(ctx context.Context, who store.ApproverIdentity) ([]string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	u := *h.endpoint
	u.RawQuery = url.Values{"provider": {who.Provider}, "subject": {who.Subject}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "error", fmt.Errorf("groupsource: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// *url.Error's text carries the full URL, subject included; keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, "error", fmt.Errorf("groupsource: request to %s: %w", h.endpoint.Host, err)
	}
	defer func() {
		// Drain a bounded tail so the connection can be reused; a failure here only
		// costs that reuse, the answer is already decided.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "not_found", nil
	case resp.StatusCode != http.StatusOK:
		// Status only: the body is the platform's and may hold internal detail.
		return nil, "error", fmt.Errorf("groupsource: %s answered HTTP %d", h.endpoint.Host, resp.StatusCode)
	}

	groups, err := decodeGroups(resp.Body)
	if err != nil {
		return nil, "error", err
	}
	return groups, "ok", nil
}

// decodeGroups reads a bounded {"groups": [...]} body. A contract violation is an
// error (vote refused), never a silently truncated list.
func decodeGroups(body io.Reader) ([]string, error) {
	limited := io.LimitReader(body, maxBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("groupsource: read response: %w", err)
	}
	if len(raw) > maxBodyBytes {
		return nil, fmt.Errorf("groupsource: response exceeds %d bytes", maxBodyBytes)
	}
	var out struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("groupsource: decode response: %w", err)
	}
	if len(out.Groups) > maxGroups {
		return nil, fmt.Errorf("groupsource: response lists %d groups (max %d)", len(out.Groups), maxGroups)
	}
	for i, g := range out.Groups {
		if g == "" || len(g) > maxGroupNameLen {
			return nil, fmt.Errorf("groupsource: groups[%d] is empty or longer than %d bytes", i, maxGroupNameLen)
		}
	}
	return out.Groups, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
