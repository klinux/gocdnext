// Package domain contains the core types that describe pipelines, materials,
// runs and everything in between. This is the canonical in-memory representation
// produced by the YAML parser and consumed by the scheduler.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// GitFingerprint returns the canonical fingerprint for a git material. Both
// the YAML parser and the webhook handler must compute identical values for
// (url, branch) so a push matches the material row stored at apply time.
// Normalization: trim whitespace, strip trailing slash, drop ".git" suffix,
// lowercase the host portion (path stays case-sensitive).
func GitFingerprint(cloneURL, branch string) string {
	u := normalizeGitURL(cloneURL)
	h := sha256.Sum256([]byte(u + "\x00" + branch))
	return hex.EncodeToString(h[:])
}

// UpstreamFingerprint is the canonical fingerprint for upstream-material
// triggers (one pipeline depending on another pipeline's stage success).
func UpstreamFingerprint(pipeline, stage string) string {
	h := sha256.Sum256([]byte("upstream\x00" + pipeline + "\x00" + stage))
	return hex.EncodeToString(h[:])
}

// CronFingerprint is the canonical fingerprint for a cron material.
func CronFingerprint(expression string) string {
	h := sha256.Sum256([]byte("cron\x00" + expression))
	return hex.EncodeToString(h[:])
}

// ManualFingerprint is the canonical fingerprint for a manual material.
// Manual materials don't carry configuration — the constant is fine.
func ManualFingerprint() string {
	h := sha256.Sum256([]byte("manual"))
	return hex.EncodeToString(h[:])
}

// NormalizeGitURL exposes the same normalization used internally by the
// fingerprint functions. Useful for matching repo URLs across sources
// (webhook payload vs scm_sources row vs material.git.url) without having
// to recompute a fingerprint just for comparison.
func NormalizeGitURL(raw string) string {
	return normalizeGitURL(raw)
}

func normalizeGitURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, "://"); i != -1 {
		rest := s[i+3:]
		if j := strings.Index(rest, "/"); j != -1 {
			host := strings.ToLower(rest[:j])
			s = s[:i+3] + host + rest[j:]
		} else {
			s = s[:i+3] + strings.ToLower(rest)
		}
	}
	return s
}

type Pipeline struct {
	ID        string
	ProjectID string
	Name      string
	Materials []Material
	Stages    []string
	Jobs      []Job
	Variables map[string]string
	Template  string
}

type MaterialType string

const (
	MaterialGit      MaterialType = "git"
	MaterialUpstream MaterialType = "upstream"
	MaterialCron     MaterialType = "cron"
	MaterialManual   MaterialType = "manual"
)

type Material struct {
	ID          string       `json:"id,omitempty"`
	Type        MaterialType `json:"type"`
	Fingerprint string       `json:"fingerprint"`
	AutoUpdate  bool         `json:"auto_update"`

	Git      *GitMaterial      `json:"git,omitempty"`
	Upstream *UpstreamMaterial `json:"upstream,omitempty"`
	Cron     *CronMaterial     `json:"cron,omitempty"`
}

// Material-config JSON tags match the YAML ones so `materials.config` in the
// DB stays human-readable (e.g. config->>'url') and queries/UI can inspect
// it without knowing Go's CamelCase field names.
type GitMaterial struct {
	URL                 string   `json:"url"`
	Branch              string   `json:"branch,omitempty"`
	Events              []string `json:"events,omitempty"`
	AutoRegisterWebhook bool     `json:"auto_register_webhook,omitempty"`
	SecretRef           string   `json:"secret_ref,omitempty"`
}

type UpstreamMaterial struct {
	Pipeline string `json:"pipeline"`
	Stage    string `json:"stage"`
	Status   string `json:"status,omitempty"`
}

type CronMaterial struct {
	Expression string `json:"expression"`
}

type Job struct {
	Name      string
	Stage     string
	Image     string
	Needs     []string
	Tasks     []Task
	Settings  map[string]string
	Variables map[string]string
	Matrix    map[string][]string
	Rules     []Rule
}

type Task struct {
	Script string
	Plugin *PluginStep
}

type PluginStep struct {
	Image    string
	Settings map[string]string
}

type Rule struct {
	IfExpr  string
	When    string
	Changes []string
}

type RunStatus string

const (
	StatusQueued   RunStatus = "queued"
	StatusRunning  RunStatus = "running"
	StatusSuccess  RunStatus = "success"
	StatusFailed   RunStatus = "failed"
	StatusCanceled RunStatus = "canceled"
	StatusSkipped  RunStatus = "skipped"
	StatusWaiting  RunStatus = "waiting"
)

type BuildCause string

const (
	CauseWebhook  BuildCause = "webhook"
	CauseUpstream BuildCause = "upstream"
	CauseManual   BuildCause = "manual"
	CauseSchedule BuildCause = "schedule"
	CausePoll     BuildCause = "poll"
)

type Revision struct {
	MaterialID string
	Revision   string
	Branch     string
	Message    string
	Author     string
	Timestamp  time.Time
}

type Run struct {
	ID          string
	PipelineID  string
	Counter     int64
	Cause       BuildCause
	Revisions   []Revision
	Status      RunStatus
	StartedAt   *time.Time
	FinishedAt  *time.Time
	TriggeredBy string
}
