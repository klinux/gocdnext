package store

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

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
	cache := map[string]RunnerProfile{}
	for _, p := range pipelines {
		for i := range p.Jobs {
			j := &p.Jobs[i]
			if j.Profile == "" {
				continue
			}
			profile, err := s.lookupProfile(ctx, cache, j.Profile)
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

func (s *Store) lookupProfile(ctx context.Context, cache map[string]RunnerProfile, name string) (RunnerProfile, error) {
	if p, ok := cache[name]; ok {
		return p, nil
	}
	p, err := s.GetRunnerProfileByName(ctx, name)
	if err != nil {
		if errors.Is(err, ErrRunnerProfileNotFound) {
			return RunnerProfile{}, fmt.Errorf("unknown runner profile %q (create it under /admin/profiles before referencing)", name)
		}
		return RunnerProfile{}, err
	}
	cache[name] = p
	return p, nil
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
