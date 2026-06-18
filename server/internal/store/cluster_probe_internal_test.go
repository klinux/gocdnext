package store

import (
	"context"
	"testing"
)

// TestProbeKubeAPI_RejectsNonHTTPS pins the defence-in-depth check:
// probeKubeAPI refuses an http://, userinfo, or empty server up front —
// no request (and so no credential) ever leaves for such a target.
func TestProbeKubeAPI_RejectsNonHTTPS(t *testing.T) {
	for _, srv := range []string{
		"http://k8s.example.com:6443",
		"https://user@k8s.example.com:6443",
		"ftp://k8s.example.com",
		"",
	} {
		res := probeKubeAPI(context.Background(), srv, nil, "tok", nil, false)
		if res.Status != ClusterProbeError {
			t.Errorf("probeKubeAPI(%q).Status = %q, want %q", srv, res.Status, ClusterProbeError)
		}
	}
}
