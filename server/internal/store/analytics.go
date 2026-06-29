package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// DoraGroup is the DORA rollup for one label-value group (e.g. team=payments)
// over the requested window.
type DoraGroup struct {
	Group             string  `json:"group"`
	DeploysSuccess    int64   `json:"deploys_success"`
	DeploysTotal      int64   `json:"deploys_total"`
	DeploysFailed     int64   `json:"deploys_failed"`
	DeployFreqPerDay  float64 `json:"deploy_freq_per_day"`
	LeadTimeP50Sec    float64 `json:"lead_time_p50_seconds"`
	ChangeFailureRate float64 `json:"change_failure_rate"`
	MTTRP50Sec        float64 `json:"mttr_p50_seconds"`
}

// LabelKeys lists the distinct label keys across all projects — the analytics
// dashboard's "group by" dimension picker.
func (s *Store) LabelKeys(ctx context.Context) ([]string, error) {
	keys, err := s.q.ListLabelKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list label keys: %w", err)
	}
	return keys, nil
}

// Environments lists the distinct deploy-environment names available as the
// analytics "environment" filter, scoped to projects carrying labelKey.
func (s *Store) Environments(ctx context.Context, labelKey string) ([]string, error) {
	envs, err := s.q.ListAnalyticsEnvironments(ctx, labelKey)
	if err != nil {
		return nil, fmt.Errorf("store: list analytics environments: %w", err)
	}
	return envs, nil
}

// DoraRollup computes the four DORA metrics for each value of `labelKey`, over
// the trailing windowDays. Deployment frequency and change-failure rate are
// derived in Go from the counts; lead time + MTTR are SQL medians.
// environment filters to a single deploy environment by name; "" means all.
func (s *Store) DoraRollup(ctx context.Context, labelKey string, windowDays int, environment string) ([]DoraGroup, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	// Trailing window: [windowDays, 0); the span (for deploy frequency) is the
	// whole window.
	return s.doraRollupWindow(ctx, labelKey, windowDays, 0, windowDays, environment)
}

// doraRollupWindow is the per-group rollup over an arbitrary [sinceDays,
// untilDays) window; spanDays is the window length used as the deploy-frequency
// denominator. The current window is [w,0) and the prior is [2w,w) — same span,
// shifted — so the movers can compare team-by-team.
func (s *Store) doraRollupWindow(ctx context.Context, labelKey string, sinceDays, untilDays, spanDays int, environment string) ([]DoraGroup, error) {
	since := dayInterval(sinceDays)
	until := dayInterval(untilDays)

	// Counts from the rollup (additive, O(days)); lead time + MTTR live (medians
	// can't be summed across daily buckets) — #128 phase 1b.
	rows, err := s.q.DoraCountsGroup(ctx, db.DoraCountsGroupParams{
		LabelKey: labelKey, Environment: environment,
		SinceDays: int32(sinceDays), UntilDays: int32(untilDays),
	})
	if err != nil {
		return nil, fmt.Errorf("store: dora counts group: %w", err)
	}
	leadRows, err := s.q.DoraLeadGroup(ctx, db.DoraLeadGroupParams{LabelKey: labelKey, SinceWindow: since, UntilWindow: until, Environment: environment})
	if err != nil {
		return nil, fmt.Errorf("store: dora lead group: %w", err)
	}
	lead := make(map[string]float64, len(leadRows))
	for _, l := range leadRows {
		lead[l.Grp] = l.LeadTimeP50S
	}
	mttrRows, err := s.q.DoraMTTR(ctx, db.DoraMTTRParams{LabelKey: labelKey, SinceWindow: since, UntilWindow: until, Environment: environment})
	if err != nil {
		return nil, fmt.Errorf("store: dora mttr: %w", err)
	}
	mttr := make(map[string]float64, len(mttrRows))
	for _, m := range mttrRows {
		mttr[m.Grp] = m.MttrP50S
	}

	out := make([]DoraGroup, 0, len(rows))
	for _, r := range rows {
		g := DoraGroup{
			Group:          r.Grp,
			DeploysSuccess: r.DeploysSuccess,
			DeploysTotal:   r.DeploysTotal,
			DeploysFailed:  r.DeploysFailed,
			LeadTimeP50Sec: lead[r.Grp],
			MTTRP50Sec:     mttr[r.Grp],
		}
		if spanDays > 0 {
			g.DeployFreqPerDay = float64(r.DeploysSuccess) / float64(spanDays)
		}
		if r.DeploysTotal > 0 {
			g.ChangeFailureRate = float64(r.DeploysFailed) / float64(r.DeploysTotal)
		}
		out = append(out, g)
	}
	return out, nil
}

// RefreshDeployDaily rebuilds the analytics_deploy_daily rollup for the trailing
// sinceDays whole calendar days (sinceDays <= 0 → full rebuild). DELETE-window +
// reinsert under the shared advisory lock — same contract as RefreshRunDaily
// (deploys are mutable via rollback/redeploy). Leader-gated; a replica that
// loses the lock no-ops.
func (s *Store) RefreshDeployDaily(ctx context.Context, sinceDays int) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: refresh deploy daily begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	got, err := q.TryRollupLock(ctx, rollupAdvisoryLockKey)
	if err != nil {
		return fmt.Errorf("store: rollup lock: %w", err)
	}
	if !got {
		return nil
	}
	if err := q.DeleteDeployDailyWindow(ctx, int32(sinceDays)); err != nil {
		return fmt.Errorf("store: refresh deploy daily delete: %w", err)
	}
	if err := q.InsertDeployDailyWindow(ctx, int32(sinceDays)); err != nil {
		return fmt.Errorf("store: refresh deploy daily insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: refresh deploy daily commit: %w", err)
	}
	return nil
}
