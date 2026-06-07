package runner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// outputsCapBytes is the hard cap on the GOCDNEXT_OUTPUT_FILE
// total size — includes keys, `=`, values, and newlines. 64KB is
// generous for the structured small-value use case (~1000
// medium-length keys) and bounded enough that a misbehaving
// plugin can't grow the JobResult message or the persisted JSONB
// row into a problem. Matches the cap declared in the issue #10
// design + the proto comment.
const outputsCapBytes = 64 * 1024

// outputsFilename is the agent-chosen filename inside
// .gocdnext/outputs/ — keyed by a short job id so concurrent
// matrix instances of the same job don't collide. Path is
// workspace-relative so it works identically regardless of which
// mount point the engine uses (Docker bridges /workspace,
// Kubernetes also /workspace, Shell host path; all of them have
// a meaningful "scriptWorkDir/<this-path>").
const outputsRelDir = ".gocdnext/outputs"

// outputsEnvName is re-exported from the engine package so the
// agent runner + engines name the env var via ONE constant. The
// engine package owns it because engines need it at RunScript
// time to inject the path-the-script-will-see (host vs container
// view); runner just references it for error messages.
const outputsEnvName = engine.OutputsEnvName

// outputsKeyRE is the POSIX env-var-name shape — same charset the
// parser enforces for the value side of `outputs:` declarations
// (server/internal/parser/parse.go::outputEnvRE). Keeping them
// aligned means a key the plugin writes either matches the
// operator's declaration or is silently dropped (filtered to the
// declared subset before ship), but never causes a parse error
// downstream.
var outputsKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// prepareOutputsFile creates the .gocdnext/outputs/<jobID>.env
// file empty + 0600 inside the script work dir, and returns the
// host-side absolute path. The agent passes the corresponding
// container-side absolute path to the job via env (GOCDNEXT_OUTPUT_FILE);
// see runner.go for the mapping.
//
// The directory mode is 0700 so a multi-job pipeline that shares
// a workspace (k8s shared mode) can't reach into another job's
// outputs file even if naming collided. The file itself is 0600
// for the same reason. The plugin process runs as root inside the
// container so it can both read and write regardless.
//
// Truncates an existing file if one is somehow left over from a
// prior run reuse — better than appending to stale state.
func prepareOutputsFile(scriptWorkDir, jobID string) (string, error) {
	dir := filepath.Join(scriptWorkDir, outputsRelDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir outputs dir: %w", err)
	}
	path := filepath.Join(dir, shortJobID(jobID)+".env")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create outputs file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close outputs file: %w", err)
	}
	return path, nil
}

// shortJobID trims a UUID-ish job id to a short, file-system-safe
// prefix. 12 hex chars is plenty of entropy for collision avoidance
// inside a single workspace (the only collision domain — outputs
// files live under scriptWorkDir which is itself per-job).
func shortJobID(id string) string {
	clean := strings.ReplaceAll(id, "-", "")
	if len(clean) > 12 {
		return clean[:12]
	}
	return clean
}

// parseOutputsFile reads, validates, and filters the
// GOCDNEXT_OUTPUT_FILE the plugin produced. Returns alias → value
// keyed by the YAML alias the operator declared. Keys present in
// the file but NOT in `declared` are silently dropped — plugins
// commonly write more state than the operator references (e.g.
// semver-bump writes NEXT, CURRENT, KIND, PREV_SHA; operator may
// only declare `next: NEXT`).
//
// Hard-fails on:
//   - file size > outputsCapBytes (including the size of unread bytes
//     past the cap so a 10MB blob can't sneak through partial reads)
//   - any line containing a NUL byte
//   - any key not matching outputsKeyRE (POSIX env-var name)
//   - duplicate keys (we reject — silently keeping first or last
//     would mask a producer bug)
//   - any declared envName missing from the file (the operator
//     promised the alias would be produced; downstream depends on
//     it — better to fail the producer than have downstream fail
//     with a confusing missing-output error far from the root cause)
//
// `declared` is the alias→envName map shipped in JobAssignment.outputs.
// Empty/nil → the file is not even read (caller skips this path
// when the job declares no outputs). Returns (nil, nil) for an
// empty file (legitimate when a plugin runs but produces nothing
// AND the operator declared no aliases — caught above).
func parseOutputsFile(hostPath string, declared map[string]string) (map[string]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("plugin did not write %s — but job declared outputs %s; check plugin docs", outputsEnvName, declaredAliasesList(declared))
		}
		return nil, fmt.Errorf("stat outputs file: %w", err)
	}
	if info.Size() > outputsCapBytes {
		return nil, fmt.Errorf("outputs file is %d bytes, cap is %d — split large blobs into artifacts instead",
			info.Size(), outputsCapBytes)
	}

	f, err := os.Open(hostPath)
	if err != nil {
		return nil, fmt.Errorf("open outputs file: %w", err)
	}
	defer func() { _ = f.Close() }()

	parsed := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(f, outputsCapBytes+1))
	// 64KB max line — the scanner's default is 64KB; outputs file is
	// itself capped at 64KB so this comfortably fits any single line.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 64*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		// Skip empty lines + comment lines (operator-friendly when
		// plugins emit "# Generated by ..." headers — see
		// gocdnext/semver-bump's output file for the convention).
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ContainsRune(line, '\x00') {
			return nil, fmt.Errorf("outputs line %d: contains NUL byte (not a valid env-var assignment)", lineNo)
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return nil, fmt.Errorf("outputs line %d: missing `=` separator (expected KEY=value)", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		value := line[eq+1:]
		// Strip a single layer of matching single or double quotes
		// around the value — plugins commonly write `KEY='value'`
		// for shell-source safety (gocdnext/semver-bump,
		// image-copy both do). Unquoted values pass through as-is.
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
				value = value[1 : len(value)-1]
			}
		}
		if !outputsKeyRE.MatchString(key) {
			return nil, fmt.Errorf("outputs line %d: key %q is not a POSIX env-var name", lineNo, key)
		}
		if _, dup := parsed[key]; dup {
			return nil, fmt.Errorf("outputs line %d: duplicate key %q (first wins is ambiguous; fix the producer)", lineNo, key)
		}
		parsed[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read outputs file: %w", err)
	}

	// Filter + rekey to alias → value.
	out := make(map[string]string, len(declared))
	var missing []string
	for alias, envName := range declared {
		v, ok := parsed[envName]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (env=%s)", alias, envName))
			continue
		}
		out[alias] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf(
			"plugin did not write declared output(s): %s — make sure the plugin writes each as KEY=value to $%s",
			strings.Join(missing, ", "), outputsEnvName)
	}
	return out, nil
}

// declaredAliasesList renders the YAML aliases for an error
// message, sorted-ish (map iteration is undefined; we just
// produce a stable-enough comma list — error text consumers
// don't depend on exact ordering, and the operator only needs to
// know WHICH aliases were promised).
func declaredAliasesList(declared map[string]string) string {
	aliases := make([]string, 0, len(declared))
	for a := range declared {
		aliases = append(aliases, a)
	}
	return strings.Join(aliases, ", ")
}
