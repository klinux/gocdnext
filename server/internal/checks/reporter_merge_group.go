package checks

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func isMergeGroupContext(ctxInfo *runContext) bool {
	return ctxInfo != nil && ctxInfo.cause == string(domain.CauseMergeGroup)
}

func mergeGroupAppDisabledError(ctxInfo *runContext) error {
	if !isMergeGroupContext(ctxInfo) {
		return nil
	}
	return errors.New("github app not configured for merge_group check reporting")
}

func mergeGroupNoInstallationError(ctxInfo *runContext) error {
	if !isMergeGroupContext(ctxInfo) {
		return nil
	}
	return fmt.Errorf("github app not installed for merge_group repo %s/%s", ctxInfo.owner, ctxInfo.repo)
}

func mergeGroupContextError(cause, format string, args ...any) error {
	if cause != string(domain.CauseMergeGroup) {
		return nil
	}
	if len(args) == 0 {
		return errors.New(format)
	}
	return fmt.Errorf(format, args...)
}

func (r *Reporter) suppressMergeGroupDestroyedCancellation(ctx context.Context, runID uuid.UUID, status string, link store.GithubCheckRun) (bool, error) {
	if status != string(domain.StatusCanceled) {
		return false, nil
	}
	suppress, err := r.store.RunCanceledByMergeGroup(ctx, runID)
	if err != nil {
		return false, err
	}
	if !suppress {
		return false, nil
	}
	r.log.Info("checks: suppressing merge_group destroyed cancellation report",
		"run_id", runID)
	if err := r.store.MarkGithubCheckRunCompleted(ctx, runID); err != nil {
		r.log.Warn("checks: mark suppressed merge_group check completed failed",
			"run_id", runID, "check_run_id", link.CheckRunID, "err", err)
	}
	return true, nil
}
