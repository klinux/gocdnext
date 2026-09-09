package runner

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Compression magic-byte prefixes. gzip is a 2-byte magic (RFC 1952);
// zstd a 4-byte one (RFC 8878). We detect by content — never by a
// filename or a stored format field — so a cache blob already sitting
// in the bucket, written before any format metadata existed, still
// restores correctly. This is what lets the isolated store flip to
// zstd (#274) without a big-bang re-tag of every existing gzip cache:
// the reader sniffs each blob and picks the decoder.
var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// podUntarCmd builds the in-pod restore command for a cache blob, chosen
// from its leading magic bytes (#274). The gzip branch is byte-identical to
// the pre-zstd command so existing caches restore exactly as before; a zstd
// blob decompresses through `zstd -dc` piped into tar.
//
// pipefail is load-bearing on the zstd path: without it, a `zstd -dc` failure
// on a truncated blob would be masked by tar exiting 0 on the partial stream,
// silently poisoning the workspace. workDir is passed as a positional shell
// arg ($1), never interpolated into the script, so an operator-controlled
// target_dir can't inject shell.
func podUntarCmd(magic []byte, workDir string) ([]string, error) {
	switch {
	case len(magic) >= 2 && magic[0] == gzipMagic[0] && magic[1] == gzipMagic[1]:
		return []string{"tar", "-xzf", "-", "-C", workDir}, nil
	case len(magic) >= 4 && magic[0] == zstdMagic[0] && magic[1] == zstdMagic[1] &&
		magic[2] == zstdMagic[2] && magic[3] == zstdMagic[3]:
		return []string{
			"sh", "-c",
			`set -o pipefail; zstd -dc -T0 | tar -xf - -C "$1"`,
			"_", workDir,
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized compression (magic %x)", magic)
	}
}

// decompressReader returns a reader that transparently inflates `r`,
// choosing gzip or zstd by peeking the leading magic bytes. The peek
// does not consume, so the chosen decoder sees the full stream (and any
// sha256 TeeReader wrapping `r` still hashes every byte). An unknown
// prefix is an error, never a silent wrong-decoder decode that would
// yield a truncated tree.
//
// The caller owns Close(): both returned readers release resources
// (gzip's reader, zstd's decoder goroutines) on Close.
func decompressReader(r io.Reader) (io.ReadCloser, error) {
	// 512 is comfortably above both magics and any gzip/zstd frame
	// header, so Peek never short-reads on a healthy stream.
	br := bufio.NewReaderSize(r, 512)
	magic, err := br.Peek(4)
	// A stream shorter than 4 bytes can't be a valid tar.gz/tar.zst
	// (the smallest empty gzip archive is ~20 bytes); surface the
	// short read rather than mis-sniffing on a 1-byte prefix.
	if err != nil && len(magic) < 2 {
		return nil, fmt.Errorf("peek compression magic: %w", err)
	}

	switch {
	case len(magic) >= 2 && magic[0] == gzipMagic[0] && magic[1] == gzipMagic[1]:
		gz, gerr := gzip.NewReader(br)
		if gerr != nil {
			return nil, fmt.Errorf("gzip reader: %w", gerr)
		}
		return gz, nil
	case len(magic) >= 4 && magic[0] == zstdMagic[0] && magic[1] == zstdMagic[1] &&
		magic[2] == zstdMagic[2] && magic[3] == zstdMagic[3]:
		zr, zerr := zstd.NewReader(br)
		if zerr != nil {
			return nil, fmt.Errorf("zstd reader: %w", zerr)
		}
		return zr.IOReadCloser(), nil
	default:
		return nil, fmt.Errorf("unrecognized compression (magic %x)", magic)
	}
}
