package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// startServices provisions a job-scoped docker bridge network and
// brings up each declared pipeline service on it with a DNS alias
// matching the service's `name`. Returns the network name (for
// the task container to join) and a cleanup closure the caller
// MUST defer — tears down containers first, then the network,
// with force-rm semantics so a partial startup still cleans.
//
// Assumes `docker` is on PATH — only called when the Docker
// engine is active (which does its own preflight check at agent
// boot). Shell-engine jobs with services: print a warning and
// return ("", noop, nil) so the script still runs, just without
// the sidecar connectivity — the service names won't resolve.
//
// Errors bubble up only on FIRST-container failure; successful
// services are included in the cleanup func so nothing leaks
// even on partial startup.
func (r *Runner) startServices(
	ctx context.Context,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) (string, func(), error) {
	services := a.GetServices()
	if len(services) == 0 {
		return "", func() {}, nil
	}

	// Job id is unique per dispatch → safe container + network
	// name, no collision between concurrent jobs on the same
	// agent. Truncated to 12 chars so `docker network create` stays
	// under its 64-char name limit when combined with the
	// container prefix.
	jobShort := shortID(a.GetJobId())
	network := "gocdnext-" + jobShort

	if _, err := exec.CommandContext(ctx, "docker", "network", "create", network).CombinedOutput(); err != nil {
		return "", func() {}, fmt.Errorf("create network %s: %w", network, err)
	}

	// Cleanup order matters: containers first (they hold references
	// to the network), then the network. We collect container ids
	// as they start so a mid-startup failure cleans what's up so
	// far without leaving orphans.
	var startedContainers []string
	cleanup := func() {
		for _, cid := range startedContainers {
			_ = exec.Command("docker", "rm", "-f", cid).Run()
		}
		_ = exec.Command("docker", "network", "rm", network).Run()
	}

	for _, svc := range services {
		name := svc.GetName()
		image := svc.GetImage()
		if name == "" || image == "" {
			cleanup()
			return "", func() {}, fmt.Errorf("service: name and image required (got name=%q image=%q)", name, image)
		}
		container := "gocdnext-" + jobShort + "-" + name
		args := []string{
			"run", "-d", "--rm",
			"--name", container,
			"--network", network,
			"--network-alias", name,
		}
		for k, v := range svc.GetEnv() {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, image)
		args = append(args, svc.GetCommand()...)

		r.emitLog(a, seq, "stdout", fmt.Sprintf("$ starting service %s (%s)", name, image))
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("start service %s: %w (docker said: %s)",
				name, err, strings.TrimSpace(string(out)))
		}
		startedContainers = append(startedContainers, container)
	}

	return network, cleanup, nil
}

// shortID trims a UUID string down to a dns-safe prefix. Docker
// network names must match a reasonably tight charset; the first
// 12 chars of a UUID hex keep uniqueness high enough for
// per-dispatch scoping on a single agent.
func shortID(id string) string {
	clean := strings.ReplaceAll(id, "-", "")
	if len(clean) > 12 {
		clean = clean[:12]
	}
	return clean
}
