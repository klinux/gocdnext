package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// OrgMetrics is the org-wide DORA rollup over a single window — the same four
// metrics as DoraGroup but without a group label. Two of these (current + prior
// window) drive the hero cards' value and "vs. prior" deltas.
type OrgMetrics struct {
	DeploysSuccess    int64   `json:"deploys_success"`
	DeploysTotal      int64   `json:"deploys_total"`
	DeploysFailed     int64   `json:"deploys_failed"`
	DeployFreqPerDay  float64 `json:"deploy_freq_per_day"`
	LeadTimeP50Sec    float64 `json:"lead_time_p50_seconds"`
	ChangeFailureRate float64 `json:"change_failure_rate"`
	MTTRP50Sec        float64 `json:"mttr_p50_seconds"`
}

// DoraDay is one daily bucket of the trailing window — the hero sparklines plot
// these. Day is an ISO date (YYYY-MM-DD). The series is dense: DoraDailySeries
// zero-fills every calendar day in the window (via generate_series), so a
// sparse window still plots an honest, non-compressed trend.
type DoraDay struct {
	Day            string  `json:"day"`
	DeploysSuccess int64   `json:"deploys_success"`
	DeploysTotal   int64   `json:"deploys_total"`
	DeploysFailed  int64   `json:"deploys_failed"`
	LeadTimeP50Sec float64 `json:"lead_time_p50_seconds"`
}

// AnalyticsOverview is the full payload the redesigned Analytics page reads in
// one shot: the org rollup (current + prior for deltas), the daily series for
// the sparklines, and the per-team leaderboard.
type AnalyticsOverview struct {
	Key         string      `json:"key"`
	WindowDays  int         `json:"window_days"`
	Environment string      `json:"environment"`
	Current     OrgMetrics  `json:"current"`
	Prior       OrgMetrics  `json:"prior"`
	Daily       []DoraDay   `json:"daily"`
	Teams       []DoraGroup `json:"teams"`
	// TeamsPrior is the same per-group rollup over the immediately preceding
	// window — the movers compare Teams vs TeamsPrior group-by-group.
	TeamsPrior []DoraGroup `json:"teams_prior"`
}

// AnalyticsOverview assembles the org rollup for `labelKey` over the trailing
// windowDays: the current window, the immediately preceding window of the same
// length (for "vs. prior" deltas), the daily series, and the per-team
// leaderboard. One read per page load.
// environment filters to a single deploy environment by name; "" means all.
func (s *Store) AnalyticsOverview(ctx context.Context, labelKey string, windowDays int, environment string) (AnalyticsOverview, error) {
	if windowDays <= 0 {
		windowDays = 30
	}

	cur, err := s.orgWindow(ctx, labelKey, windowDays, 0, environment)
	if err != nil {
		return AnalyticsOverview{}, err
	}
	// Prior window: [2×window, window) trailing from now — same span, shifted.
	prior, err := s.orgWindow(ctx, labelKey, 2*windowDays, windowDays, environment)
	if err != nil {
		return AnalyticsOverview{}, err
	}

	daysRows, err := s.q.DoraDailySeries(ctx, db.DoraDailySeriesParams{
		LabelKey:    labelKey,
		SinceWindow: dayInterval(windowDays),
		Environment: environment,
	})
	if err != nil {
		return AnalyticsOverview{}, fmt.Errorf("store: dora daily series: %w", err)
	}
	daily := make([]DoraDay, 0, len(daysRows))
	for _, d := range daysRows {
		day := ""
		if d.Day.Valid {
			day = d.Day.Time.Format("2006-01-02")
		}
		daily = append(daily, DoraDay{
			Day:            day,
			DeploysSuccess: d.DeploysSuccess,
			DeploysTotal:   d.DeploysTotal,
			DeploysFailed:  d.DeploysFailed,
			LeadTimeP50Sec: d.LeadTimeP50S,
		})
	}

	teams, err := s.doraRollupWindow(ctx, labelKey, windowDays, 0, windowDays, environment)
	if err != nil {
		return AnalyticsOverview{}, err
	}
	teamsPrior, err := s.doraRollupWindow(ctx, labelKey, 2*windowDays, windowDays, windowDays, environment)
	if err != nil {
		return AnalyticsOverview{}, err
	}

	return AnalyticsOverview{
		Key:         labelKey,
		WindowDays:  windowDays,
		Environment: environment,
		Current:     metricsFromWindow(cur, windowDays),
		Prior:       metricsFromWindow(prior, windowDays),
		Daily:       daily,
		Teams:       teams,
		TeamsPrior:  teamsPrior,
	}, nil
}

// orgWindowRaw holds the unprocessed counts + medians for one window before
// frequency/CFR are derived.
type orgWindowRaw struct {
	success, total, failed int64
	leadP50, mttrP50       float64
}

// orgWindow runs the org-wide counts + lead-time p50 and the MTTR p50 for the
// trailing [sinceDays, untilDays) window. untilDays=0 means "up to now".
func (s *Store) orgWindow(ctx context.Context, labelKey string, sinceDays, untilDays int, environment string) (orgWindowRaw, error) {
	agg, err := s.q.DoraWindowAgg(ctx, db.DoraWindowAggParams{
		LabelKey:    labelKey,
		SinceWindow: dayInterval(sinceDays),
		UntilWindow: dayInterval(untilDays),
		Environment: environment,
	})
	if err != nil {
		return orgWindowRaw{}, fmt.Errorf("store: dora window agg: %w", err)
	}
	mttr, err := s.q.DoraWindowMTTR(ctx, db.DoraWindowMTTRParams{
		LabelKey:    labelKey,
		SinceWindow: dayInterval(sinceDays),
		UntilWindow: dayInterval(untilDays),
		Environment: environment,
	})
	if err != nil {
		return orgWindowRaw{}, fmt.Errorf("store: dora window mttr: %w", err)
	}
	return orgWindowRaw{
		success: agg.DeploysSuccess,
		total:   agg.DeploysTotal,
		failed:  agg.DeploysFailed,
		leadP50: agg.LeadTimeP50S,
		mttrP50: mttr,
	}, nil
}

// metricsFromWindow derives deployment frequency (successes per day over the
// window span) and change-failure rate from the raw counts.
func metricsFromWindow(r orgWindowRaw, windowDays int) OrgMetrics {
	m := OrgMetrics{
		DeploysSuccess: r.success,
		DeploysTotal:   r.total,
		DeploysFailed:  r.failed,
		LeadTimeP50Sec: r.leadP50,
		MTTRP50Sec:     r.mttrP50,
	}
	if windowDays > 0 {
		m.DeployFreqPerDay = float64(r.success) / float64(windowDays)
	}
	if r.total > 0 {
		m.ChangeFailureRate = float64(r.failed) / float64(r.total)
	}
	return m
}

func dayInterval(days int) pgtype.Interval {
	return pgtype.Interval{Days: int32(days), Valid: true}
}
