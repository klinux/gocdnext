package runs

import "testing"

// TestDownloadFilename covers the codec-aware saved-as name (#283): a zstd
// artifact must download as `.tar.zst` (extractable with `tar xf`/`--zstd`),
// a gzip one as `.tar.gz`, and an unknown/empty content type falls back to
// gzip (old rows predate the content_type column and read back as gzip).
func TestDownloadFilename(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		contentType string
		want        string
	}{
		{"file gzip", "bin/gocdnext-server", "application/gzip", "gocdnext-server.tar.gz"},
		{"file zstd", "bin/gocdnext-server", "application/zstd", "gocdnext-server.tar.zst"},
		{"dir gzip", "web/.next/standalone/", "application/gzip", "standalone.tar.gz"},
		{"dir zstd", "web/.next/standalone/", "application/zstd", "standalone.tar.zst"},
		{"empty path gzip", "", "application/gzip", "artifact.tar.gz"},
		{"empty path zstd", "", "application/zstd", "artifact.tar.zst"},
		{"unknown codec falls back to gzip", "dist", "", "dist.tar.gz"},
		{"garbage codec falls back to gzip", "dist", "application/x-brotli", "dist.tar.gz"},
		{"root slash", "/", "application/zstd", "artifact.tar.zst"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := downloadFilename(tt.path, tt.contentType); got != tt.want {
				t.Errorf("downloadFilename(%q, %q) = %q, want %q", tt.path, tt.contentType, got, tt.want)
			}
		})
	}
}
