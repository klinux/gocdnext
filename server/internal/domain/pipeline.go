// Package domain contains the core types that describe pipelines, materials,
// runs and everything in between. This is the canonical in-memory representation
// produced by the YAML parser and consumed by the scheduler.
package domain

import "time"

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
	ID          string
	Type        MaterialType
	Fingerprint string
	AutoUpdate  bool

	Git      *GitMaterial
	Upstream *UpstreamMaterial
	Cron     *CronMaterial
}

type GitMaterial struct {
	URL                 string
	Branch              string
	Events              []string
	AutoRegisterWebhook bool
	SecretRef           string
}

type UpstreamMaterial struct {
	Pipeline string
	Stage    string
	Status   string
}

type CronMaterial struct {
	Expression string
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
