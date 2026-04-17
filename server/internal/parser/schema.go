// Package parser reads `.gocdnext.yaml` files and produces domain.Pipeline.
//
// The YAML schema borrows from three tools:
//   - GoCD: stages/jobs/tasks, upstream materials, templates
//   - GitLab CI: stages list, rules, needs, extends/include, parallel matrix
//   - Woodpecker: plugin = container + settings (PLUGIN_* env vars)
package parser

// File is the top-level `.gocdnext.yaml` structure.
type File struct {
	Version   string            `yaml:"version,omitempty"` // reserved for future
	Include   []Include         `yaml:"include,omitempty"`
	Materials []MaterialSpec    `yaml:"materials"`
	Stages    []string          `yaml:"stages"`
	Variables map[string]string `yaml:"variables,omitempty"`
	Template  string            `yaml:"template,omitempty"`
	Jobs      map[string]JobDef `yaml:"jobs"`
}

type Include struct {
	Local    string `yaml:"local,omitempty"`
	Remote   string `yaml:"remote,omitempty"`
	Template string `yaml:"template,omitempty"`
}

// MaterialSpec — one of the fields must be set.
type MaterialSpec struct {
	Git      *GitSpec      `yaml:"git,omitempty"`
	Upstream *UpstreamSpec `yaml:"upstream,omitempty"`
	Cron     *CronSpec     `yaml:"cron,omitempty"`
	Manual   bool          `yaml:"manual,omitempty"`
}

type GitSpec struct {
	URL                 string   `yaml:"url"`
	Branch              string   `yaml:"branch,omitempty"`
	On                  []string `yaml:"on,omitempty"` // push, pull_request
	AutoRegisterWebhook bool     `yaml:"auto_register_webhook,omitempty"`
	SecretRef           string   `yaml:"secret_ref,omitempty"`
}

type UpstreamSpec struct {
	Pipeline string `yaml:"pipeline"`
	Stage    string `yaml:"stage"`
	Status   string `yaml:"status,omitempty"`
}

type CronSpec struct {
	Expression string `yaml:"expression"`
}

// JobDef — definition of one job. Supports `extends` via YAML anchors.
type JobDef struct {
	Stage     string            `yaml:"stage"`
	Image     string            `yaml:"image,omitempty"`
	Extends   string            `yaml:"extends,omitempty"`
	Needs     []string          `yaml:"needs,omitempty"`
	Script    []string          `yaml:"script,omitempty"`
	Settings  map[string]string `yaml:"settings,omitempty"` // plugin step
	Variables map[string]string `yaml:"variables,omitempty"`
	Cache     *Cache            `yaml:"cache,omitempty"`
	Artifacts *Artifacts        `yaml:"artifacts,omitempty"`
	Parallel  *Parallel         `yaml:"parallel,omitempty"`
	Rules     []RuleDef         `yaml:"rules,omitempty"`
	When      *WhenDef          `yaml:"when,omitempty"`
	Timeout   string            `yaml:"timeout,omitempty"`
	Retry     int               `yaml:"retry,omitempty"`
}

type Cache struct {
	Key   string   `yaml:"key,omitempty"`
	Paths []string `yaml:"paths"`
}

type Artifacts struct {
	Paths    []string `yaml:"paths"`
	ExpireIn string   `yaml:"expire_in,omitempty"`
	When     string   `yaml:"when,omitempty"` // on_success|on_failure|always
}

type Parallel struct {
	Matrix []map[string][]string `yaml:"matrix,omitempty"`
	Count  int                   `yaml:"count,omitempty"`
}

type RuleDef struct {
	If      string   `yaml:"if,omitempty"`
	Changes []string `yaml:"changes,omitempty"`
	Exists  []string `yaml:"exists,omitempty"`
	When    string   `yaml:"when,omitempty"` // always|manual|never|on_success
}

type WhenDef struct {
	Status []string `yaml:"status,omitempty"` // success|failure|always
	Branch []string `yaml:"branch,omitempty"`
	Event  []string `yaml:"event,omitempty"`
}
