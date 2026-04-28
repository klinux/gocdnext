// Package logarchive implements cold-archive of job log_lines into
// the artifact object store. One archive per job_run so the read
// path can fetch a specific job without parsing a tarball.
//
// Format (gzipped):
//
//	<seq>\t<at-RFC3339Nano>\t<stream>\t<text>\n
//
// Tab-separated, NL-terminated, one line per row in the original
// table. Text is byte-faithful — newlines inside the text are
// rare (the agent already splits at newlines before sending), but
// when present they're preserved verbatim because the parser
// reads exactly four tabs and then takes the rest of the line up
// to the trailing \n. A simpler `bufio.Scanner` would split on
// internal newlines; the parser here handles that explicitly.
package logarchive

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Line is the format-level representation of one log row. Kept
// store-independent so the store can import logarchive (for
// ReadArchive on the fallback read path) without re-introducing
// the import cycle that would form if Line referenced
// store.LogLine.
type Line struct {
	Seq    int64
	At     time.Time
	Stream string
	Text   string
}

// WriteArchive streams `lines` into `w` gzipped, in the format
// above. Caller closes `w`. Returns the byte count written
// post-gzip so the archiver can report it on the job_run.
func WriteArchive(w io.Writer, lines []Line) (int64, error) {
	cw := &countWriter{w: w}
	gz := gzip.NewWriter(cw)
	bw := bufio.NewWriterSize(gz, 64*1024)
	for _, l := range lines {
		if _, err := bw.WriteString(strconv.FormatInt(l.Seq, 10)); err != nil {
			return cw.n, err
		}
		if err := bw.WriteByte('\t'); err != nil {
			return cw.n, err
		}
		at := l.At.UTC().Format(time.RFC3339Nano)
		if _, err := bw.WriteString(at); err != nil {
			return cw.n, err
		}
		if err := bw.WriteByte('\t'); err != nil {
			return cw.n, err
		}
		if _, err := bw.WriteString(l.Stream); err != nil {
			return cw.n, err
		}
		if err := bw.WriteByte('\t'); err != nil {
			return cw.n, err
		}
		// Text MAY contain tabs — escape \t within text as \\t and
		// \n as \\n so the four-field parser stays unambiguous.
		// Most log lines are tab-free; the cost is paid only when
		// not.
		if _, err := bw.WriteString(escape(l.Text)); err != nil {
			return cw.n, err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return cw.n, err
		}
	}
	if err := bw.Flush(); err != nil {
		return cw.n, err
	}
	if err := gz.Close(); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

// ReadArchive parses what WriteArchive produced. Lines come back
// in the order they were written. A truncated archive returns the
// successfully-parsed prefix plus an error — callers can choose
// whether to surface the partial result or treat it as a hard
// failure.
func ReadArchive(r io.Reader) ([]Line, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("logarchive: gzip header: %w", err)
	}
	defer gz.Close()
	br := bufio.NewReader(gz)
	var out []Line
	for ln := 1; ; ln++ {
		raw, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) {
			if raw == "" {
				return out, nil
			}
			// Trailing line without newline — accept.
		} else if err != nil {
			return out, fmt.Errorf("logarchive: read line %d: %w", ln, err)
		}
		raw = strings.TrimRight(raw, "\n")
		if raw == "" {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			continue
		}
		l, ok := parseLine(raw)
		if !ok {
			return out, fmt.Errorf("logarchive: malformed line %d", ln)
		}
		out = append(out, l)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
	}
}

// parseLine splits a raw `<seq>\t<at>\t<stream>\t<text>` line
// into the four columns. Text may contain `\\t` / `\\n` escapes;
// they're decoded back here.
func parseLine(s string) (Line, bool) {
	parts := strings.SplitN(s, "\t", 4)
	if len(parts) != 4 {
		return Line{}, false
	}
	seq, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Line{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return Line{}, false
	}
	return Line{
		Seq:    seq,
		At:     at.UTC(),
		Stream: parts[2],
		Text:   unescape(parts[3]),
	}, true
}

// escape replaces tab and newline with literal `\t` / `\n`. Backslash
// stays — the unescape pass only walks `\` followed by a known
// escape character, so a stray backslash is left alone (round-trips
// through unchanged).
func escape(s string) string {
	if !strings.ContainsAny(s, "\t\n\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// countWriter wraps an io.Writer and counts bytes that go through
// — the archiver reports compressed size to operators so a 1 GB
// log job stands out in dashboards.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
