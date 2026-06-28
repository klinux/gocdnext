package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

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

// DoraRollup computes the four DORA metrics for each value of `labelKey`, over
// the trailing windowDays. Deployment frequency and change-failure rate are
// derived in Go from the counts; lead time + MTTR are SQL medians.
func (s *Store) DoraRollup(ctx context.Context, labelKey string, windowDays int) ([]DoraGroup, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	iv := pgtype.Interval{Days: int32(windowDays), Valid: true}

	rows, err := s.q.DoraRollup(ctx, db.DoraRollupParams{LabelKey: labelKey, SinceWindow: iv})
	if err != nil {
		return nil, fmt.Errorf("store: dora rollup: %w", err)
	}
	mttrRows, err := s.q.DoraMTTR(ctx, db.DoraMTTRParams{LabelKey: labelKey, SinceWindow: iv})
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
			LeadTimeP50Sec: r.LeadTimeP50S,
			MTTRP50Sec:     mttr[r.Grp],
		}
		g.DeployFreqPerDay = float64(r.DeploysSuccess) / float64(windowDays)
		if r.DeploysTotal > 0 {
			g.ChangeFailureRate = float64(r.DeploysFailed) / float64(r.DeploysTotal)
		}
		out = append(out, g)
	}
	return out, nil
}
