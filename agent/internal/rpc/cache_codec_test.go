package rpc

import (
	"strings"
	"testing"
)

// TestCacheTarScript_ByCodec pins the two store scripts. gzip MUST stay the
// pre-zstd script (`tar -czf`, with exec) so nothing changes for the default;
// zstd pipes tar into `zstd -T0` under pipefail so a compressor failure on a
// truncated stream can't be masked by tar exiting 0 (#274).
func TestCacheTarScript_ByCodec(t *testing.T) {
	gz := cacheTarScript(codecGzip)
	if !strings.Contains(gz, "tar -czf") {
		t.Errorf("gzip script should use `tar -czf`, got: %s", gz)
	}
	if strings.Contains(gz, "zstd") {
		t.Errorf("gzip script must not mention zstd, got: %s", gz)
	}

	zs := cacheTarScript(codecZstd)
	for _, must := range []string{"set -o pipefail", "zstd -T0", "tar -cf -"} {
		if !strings.Contains(zs, must) {
			t.Errorf("zstd script missing %q, got: %s", must, zs)
		}
	}
	if strings.Contains(zs, "-czf") {
		t.Errorf("zstd script must not gzip via `-czf`, got: %s", zs)
	}
}

func TestCacheContentType_ByCodec(t *testing.T) {
	if got := cacheContentType(codecGzip); got != "application/gzip" {
		t.Errorf("gzip content-type = %q", got)
	}
	if got := cacheContentType(codecZstd); got != "application/zstd" {
		t.Errorf("zstd content-type = %q", got)
	}
}

// TestUseCompression_FailSafeToGzip: an unset or garbage codec must fall back
// to gzip, never silently pick a codec the housekeeper image can't run.
func TestUseCompression_FailSafeToGzip(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", codecGzip},
		{"gzip", codecGzip},
		{"ZSTD", codecZstd},
		{"zstd", codecZstd},
		{"lz4", codecGzip},
		{"  zstd ", codecZstd},
	}
	for _, tt := range tests {
		c := &CacheClient{}
		c.UseCompression(tt.in)
		if c.compression != tt.want {
			t.Errorf("UseCompression(%q) → %q, want %q", tt.in, c.compression, tt.want)
		}
	}
}
