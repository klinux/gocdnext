package retention

import "context"

// reconcileArchives runs the two cold-archive reconciliation
// passes on the same tick:
//
//  1. Re-submit: find terminal jobs whose logs_archive_uri is
//     still NULL past the grace window and push them back into the
//     archiver. The submit path is nil-safe and idempotent — the
//     archiver dedupes by skipping jobs whose URI is set when its
//     worker picks them up.
//  2. Orphan cleanup: jobs whose URI is stamped but log_lines rows
//     still exist (DELETE step failed after the URI update). The
//     read path already serves from the archive, so the rows are
//     pure cost; DELETE them now.
//
// Both passes are bounded by archiveBatch so a backlog can't
// dominate the tick. Errors are warn-logged and the next tick
// retries — no per-row backoff needed because archiveGrace already
// keeps the queue sane.
func (s *Sweeper) reconcileArchives(ctx context.Context, stats *SweepStats) {
	jobs, err := s.store.ListJobsNeedingArchive(ctx, s.archiveGrace, s.archiveBatch)
	if err != nil {
		s.log.Warn("retention: list jobs needing archive", "err", err)
	}
	for _, j := range jobs {
		if !s.archiveResolver(j.ProjectFlag) {
			// Project opted out (or global=off when the resolver
			// closes over the global policy). Skip — the URI stays
			// NULL forever, which is correct.
			continue
		}
		s.archiver.Submit(j.JobRunID)
		stats.ArchivesReSubmitted++
	}

	orphans, err := s.store.ListOrphanedArchivedJobs(ctx, s.archiveBatch)
	if err != nil {
		s.log.Warn("retention: list orphaned archives", "err", err)
		return
	}
	for _, jobID := range orphans {
		if err := s.store.DeleteLogLinesByJob(ctx, jobID); err != nil {
			s.log.Warn("retention: delete orphan log_lines",
				"job_run_id", jobID, "err", err)
			continue
		}
		stats.ArchiveOrphansDeleted++
	}
	if stats.ArchivesReSubmitted > 0 || stats.ArchiveOrphansDeleted > 0 {
		s.log.Info("retention: archive reconcile",
			"resubmitted", stats.ArchivesReSubmitted,
			"orphans_deleted", stats.ArchiveOrphansDeleted)
	}
}
