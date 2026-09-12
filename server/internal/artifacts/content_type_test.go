package artifacts

import (
	"bytes"
	"context"
	"testing"
)

// TestDetectContentType pins the codec sniff: zstd magic → application/zstd,
// everything else (gzip, short, junk) → application/gzip. The default is
// deliberate — the artifact store only ever writes gzip or zstd, and gzip is
// the safe legacy assumption for anything that isn't clearly zstd (#283).
func TestDetectContentType(t *testing.T) {
	tests := []struct {
		name  string
		magic []byte
		want  string
	}{
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}, "application/zstd"},
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, "application/gzip"},
		{"short", []byte{0x28, 0xb5}, "application/gzip"},
		{"empty", []byte{}, "application/gzip"},
		{"junk", []byte{0x00, 0x01, 0x02, 0x03}, "application/gzip"},
		{"zstd-prefix-but-3-bytes", []byte{0x28, 0xb5, 0x2f}, "application/gzip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectContentType(tt.magic); got != tt.want {
				t.Errorf("detectContentType(%x) = %q, want %q", tt.magic, got, tt.want)
			}
		})
	}
}

// TestInspectObject_SniffsCodec proves InspectObject derives the content type
// from the object's leading bytes (server-observed, not agent-trusted) while
// still computing size+sha over the whole stream — same single read. Uses raw
// magic-prefixed payloads; InspectObject never decompresses, only sniffs.
func TestInspectObject_SniffsCodec(t *testing.T) {
	fs := mustFS(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"zstd", append([]byte{0x28, 0xb5, 0x2f, 0xfd}, []byte("zstd-body-bytes")...), "application/zstd"},
		{"gzip", append([]byte{0x1f, 0x8b, 0x08, 0x00}, []byte("gzip-body-bytes")...), "application/gzip"},
		{"legacy plain (old artifact) → gzip", []byte("no magic here"), "application/gzip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "run/x/job/y/" + tc.name
			if _, err := fs.Put(ctx, key, bytes.NewReader(tc.payload)); err != nil {
				t.Fatalf("put: %v", err)
			}
			got, err := InspectObject(ctx, fs, key)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if got.ContentType != tc.want {
				t.Errorf("ContentType = %q, want %q", got.ContentType, tc.want)
			}
			if got.Size != int64(len(tc.payload)) {
				t.Errorf("size = %d, want %d (sniff must not consume the stream)", got.Size, len(tc.payload))
			}
		})
	}
}
