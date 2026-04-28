package logarchive_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/logarchive"
)

// TestArchiveRoundTrip drives WriteArchive → ReadArchive end-to-end
// with content that exercises every branch of the escape pass:
// plain text, embedded tab, embedded newline, lone backslash. The
// reader must hand back exactly what the writer was given.
func TestArchiveRoundTrip(t *testing.T) {
	at := time.Date(2026, 4, 28, 10, 11, 12, 345000000, time.UTC)
	in := []logarchive.Line{
		{Seq: 1, At: at, Stream: "stdout", Text: "plain hello"},
		{Seq: 2, At: at.Add(time.Second), Stream: "stderr", Text: "tab\there"},
		{Seq: 3, At: at.Add(2 * time.Second), Stream: "stdout", Text: "multi\nline\ntext"},
		{Seq: 4, At: at.Add(3 * time.Second), Stream: "stdout", Text: `c:\windows\path`},
		{Seq: 5, At: at.Add(4 * time.Second), Stream: "stdout", Text: ""},
	}

	var buf bytes.Buffer
	n, err := logarchive.WriteArchive(&buf, in)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n <= 0 {
		t.Errorf("written byte count = %d, want positive", n)
	}

	out, err := logarchive.ReadArchive(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Seq != in[i].Seq ||
			!out[i].At.Equal(in[i].At) ||
			out[i].Stream != in[i].Stream ||
			out[i].Text != in[i].Text {
			t.Errorf("line %d round-trip mismatch:\n got=%+v\nwant=%+v", i, out[i], in[i])
		}
	}
}

func TestArchiveEmpty(t *testing.T) {
	var buf bytes.Buffer
	if _, err := logarchive.WriteArchive(&buf, nil); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	out, err := logarchive.ReadArchive(&buf)
	if err != nil {
		t.Fatalf("read empty: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("read empty returned %d lines, want 0", len(out))
	}
}
