package runlocal

import (
	"strings"
	"testing"
)

// Review-round HIGH: env VALUES must never reach the docker argv —
// only `-e NAME` references; values ride the docker CLI's own env.
func TestJobRunArgs_NoEnvValuesOnArgv(t *testing.T) {
	j := PlannedJob{
		Name:   "build",
		Image:  "alpine:3.20",
		Script: []string{"true"},
	}
	names, kv := envArgs(map[string]string{
		"MY_TOKEN": "s3cr3t-value",
		"PLAIN":    "ok",
	})
	args := jobRunArgs(j, "/tmp/ws", "net", names)

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "s3cr3t-value") {
		t.Fatalf("secret value leaked to argv: %s", joined)
	}
	if !strings.Contains(joined, "-e MY_TOKEN") || strings.Contains(joined, "MY_TOKEN=") {
		t.Fatalf("expected name-only -e reference: %s", joined)
	}
	// The value must be available for cmd.Env propagation.
	found := false
	for _, pair := range kv {
		if pair == "MY_TOKEN=s3cr3t-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("value missing from cmd.Env pairs: %v", kv)
	}
}
