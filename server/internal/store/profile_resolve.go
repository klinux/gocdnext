package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// DefaultRunnerProfileName is the conventional name probed as a
// fallback when a job declares no `profile:` explicitly. When the
// admin has created (or seeded) a profile with this name, jobs that
// declared nothing pick up its RESOURCE BOUNDS only — image, tags,
// env, secrets, and caps stay strictly opt-in via explicit
// `profile: default` so silent fallback can't surprise pipelines
// that intentionally ran unbounded before.
//
// When no profile named "default" exists, fallback is a no-op and
// the pre-fallback shape (zero bounds, container without
// resources:) is preserved.
const DefaultRunnerProfileName = "default"

// ResolveProfiles is the apply-time validator that turns a parsed
// pipeline's profile names into concrete policy. For each job that
// names a runner profile, it:
//
//   - looks the profile up by name (404 → typed error);
//   - merges profile.Tags into Job.Tags (union; user-typed tags win
//     dedup, profile tags are appended only when not already there);
//   - fills Job.Image from profile.DefaultImage when the job left
//     image empty (engines without an image → script falls back to
//     shell engine; the field is just a strong hint);
//   - fills empty fields of Job.Resources from the profile's
//     default_*_request/limit;
//   - validates user-set Job.Resources against the profile's
//     max_cpu / max_mem caps. Hard fail on first violation, with
//     a job-name-prefixed message for the YAML author.
//
// Mutates pipelines in place. Returns the first violation it sees
// — apply is all-or-nothing, no point reporting cap errors that
// might already be obviated by an earlier reject.
func (s *Store) ResolveProfiles(ctx context.Context, pipelines []*domain.Pipeline) error {
	return resolveProfilesQ(ctx, s.q, pipelines)
}

// resolveProfilesQ is the querier-parameterised body of ResolveProfiles so a
// caller INSIDE a transaction (CreatePRHeadRun) resolves profile image/resource
// defaults + caps on the SAME tx querier. Without this a PR-head snapshot would
// carry unresolved resources and bypass the admin profile cap at dispatch (which
// reads resources straight from the snapshot). Mutates pipelines in place.
func resolveProfilesQ(ctx context.Context, q *db.Queries, pipelines []*domain.Pipeline) error {
	cache := map[string]RunnerProfile{}
	lookup := func(name string) (RunnerProfile, error) {
		if p, ok := cache[name]; ok {
			return p, nil
		}
		row, err := q.GetRunnerProfileByName(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return RunnerProfile{}, fmt.Errorf("unknown runner profile %q (create it under /admin/profiles before referencing)", name)
		}
		if err != nil {
			return RunnerProfile{}, err
		}
		p, cerr := runnerProfileFromRow(row)
		if cerr != nil {
			return RunnerProfile{}, cerr
		}
		cache[name] = p
		return p, nil
	}

	// Probe for the "default" profile once. Fallback for jobs that declared no
	// profile: — applies ONLY resource bounds, never image/tags/env/secrets/caps.
	// Missing default = continue with the pre-fallback shape. Real DB errors
	// still propagate.
	var defaultProfile *RunnerProfile
	if row, err := q.GetRunnerProfileByName(ctx, DefaultRunnerProfileName); err == nil {
		p, cerr := runnerProfileFromRow(row)
		if cerr != nil {
			return cerr
		}
		defaultProfile = &p
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("probe default runner profile: %w", err)
	}

	for _, p := range pipelines {
		for i := range p.Jobs {
			j := &p.Jobs[i]
			if j.Profile == "" {
				if defaultProfile != nil {
					fillProfileResources(j, *defaultProfile)
				}
				continue
			}
			profile, err := lookup(j.Profile)
			if err != nil {
				return fmt.Errorf("pipeline %q: job %q: %w", p.Name, j.Name, err)
			}
			mergeProfileTags(j, profile)
			fillProfileImage(j, profile)
			fillProfileResources(j, profile)
			if err := enforceProfileCaps(j, profile); err != nil {
				return fmt.Errorf("pipeline %q: job %q: %w", p.Name, j.Name, err)
			}
		}
	}
	return nil
}

func mergeProfileTags(j *domain.Job, p RunnerProfile) {
	if len(p.Tags) == 0 {
		return
	}
	have := make(map[string]struct{}, len(j.Tags))
	for _, t := range j.Tags {
		have[t] = struct{}{}
	}
	for _, t := range p.Tags {
		if _, dup := have[t]; dup {
			continue
		}
		j.Tags = append(j.Tags, t)
		have[t] = struct{}{}
	}
}

func fillProfileImage(j *domain.Job, p RunnerProfile) {
	if j.Image == "" {
		j.Image = p.DefaultImage
	}
}

func fillProfileResources(j *domain.Job, p RunnerProfile) {
	if j.Resources.Requests.CPU == "" {
		j.Resources.Requests.CPU = p.DefaultCPURequest
	}
	if j.Resources.Requests.Memory == "" {
		j.Resources.Requests.Memory = p.DefaultMemRequest
	}
	if j.Resources.Limits.CPU == "" {
		j.Resources.Limits.CPU = p.DefaultCPULimit
	}
	if j.Resources.Limits.Memory == "" {
		j.Resources.Limits.Memory = p.DefaultMemLimit
	}
}

// enforceProfileCaps clamps user-set requests/limits against the
// profile cap. The cap applies to BOTH requests and limits — a
// runaway request that exceeds the cap would fail at scheduler
// time anyway (kubelet rejects it) and the cap is the policy
// surface admins use to bound greedy YAMLs. requests > limits is
// also caught here since it's a misconfiguration that any engine
// will refuse to honour.
func enforceProfileCaps(j *domain.Job, p RunnerProfile) error {
	if err := compareQuantities("requests.cpu", j.Resources.Requests.CPU, "limits.cpu", j.Resources.Limits.CPU, leq); err != nil {
		return err
	}
	if err := compareQuantities("requests.memory", j.Resources.Requests.Memory, "limits.memory", j.Resources.Limits.Memory, leq); err != nil {
		return err
	}
	if err := compareCap("requests.cpu", j.Resources.Requests.CPU, "max_cpu", p.MaxCPU); err != nil {
		return err
	}
	if err := compareCap("limits.cpu", j.Resources.Limits.CPU, "max_cpu", p.MaxCPU); err != nil {
		return err
	}
	if err := compareCap("requests.memory", j.Resources.Requests.Memory, "max_mem", p.MaxMem); err != nil {
		return err
	}
	if err := compareCap("limits.memory", j.Resources.Limits.Memory, "max_mem", p.MaxMem); err != nil {
		return err
	}
	return nil
}

// leq returns true when a ≤ b (k8s Quantity ordering).
func leq(a, b resource.Quantity) bool { return a.Cmp(b) <= 0 }

func compareQuantities(aLabel, aRaw, bLabel, bRaw string, ok func(a, b resource.Quantity) bool) error {
	if aRaw == "" || bRaw == "" {
		return nil
	}
	a, err := resource.ParseQuantity(aRaw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", aLabel, aRaw, err)
	}
	b, err := resource.ParseQuantity(bRaw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", bLabel, bRaw, err)
	}
	if !ok(a, b) {
		return fmt.Errorf("%s (%s) must be ≤ %s (%s)", aLabel, aRaw, bLabel, bRaw)
	}
	return nil
}

// compareCap is the cap-only variant: when capRaw is empty, no
// limit is enforced (admin opted out for this profile).
func compareCap(label, raw, capLabel, capRaw string) error {
	if raw == "" || capRaw == "" {
		return nil
	}
	v, err := resource.ParseQuantity(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", label, raw, err)
	}
	cap, err := resource.ParseQuantity(capRaw)
	if err != nil {
		return fmt.Errorf("invalid profile %s %q: %w", capLabel, capRaw, err)
	}
	if v.Cmp(cap) > 0 {
		return fmt.Errorf("%s %s exceeds profile %s %s", label, raw, capLabel, capRaw)
	}
	return nil
}
