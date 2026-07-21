-- +goose Up
-- +goose StatementBegin

-- Gate-driven Argo Rollouts control (ADR-0001 Phase 2, PR2). Builds on the
-- observe-only columns (00071). A rollout-aware target may carry a governing_gate:
-- when its canary pauses INDEFINITELY (a `pause: {}` step, no duration), gocdnext ARMS
-- an approval gate on the in-flight deploy_watch. Approve -> Promote (advance a step),
-- reject -> Abort (traffic back to stable). governing_gate PRESENT <=> control mode;
-- rollout_aware WITHOUT governing_gate stays observe-only.

-- deploy_targets: the gate config. JSONB = {approvers[], approver_groups[], required,
-- description}. NULL => no gate (observe-only or non-rollout). Editing this column (and
-- the rollout routing on a gated target) is admin-only — enforced in the registrar
-- (separation of duties: a maintainer must not be able to reroute around a gate).
ALTER TABLE deploy_targets
    ADD COLUMN governing_gate JSONB NULL;

-- deploy_watches: the gate state, in four groups.
--
--   CONFIG — denormalized from the target's governing_gate at watch CREATION, an
--     immutable per-deploy snapshot (a mid-flight target edit must not change an
--     in-flight deploy's gate). gate_required IS NOT NULL <=> this deploy is gated.
--     ClearRolloutGate (per step) must NOT null these — they span the whole deploy.
--   PER-ARM — stamped when a step pauses indefinitely; nulled by ClearRolloutGate when
--     the rollout leaves the step, re-stamped fresh on the next pause. gate_id is the
--     anti-stale token (fresh per arm; approve/reject must carry + match it under the
--     row lock, else 409). gate_rollout_{cluster,namespace,name} is the RESOLVED
--     Rollout identity PINNED at arm time — Promote/Abort act on it, never a
--     re-discovery, so `.status.resources[]` drift can't redirect the effect.
--   DECISION — gate_decision (approved|rejected) + who/when. The terminal decision (not
--     the later action) ends the awaiting-human window and resumes the deadline.
--   ACTION — watcher actuation timestamps (NOT observed from the cluster). gate_actioned_at
--     guards the gated promote/reject against re-issue; rollout_abort_actioned_at is the
--     gate-INDEPENDENT anti-re-abort for the cancel/supersede path (a non-gated rollout
--     can be aborted too), so it is a separate column.
ALTER TABLE deploy_watches
    -- CONFIG (creation-time, whole-deploy)
    ADD COLUMN gate_approvers            TEXT[] NULL,
    ADD COLUMN gate_approver_groups      TEXT[] NULL,
    ADD COLUMN gate_required             INT NULL,
    ADD COLUMN gate_description          TEXT NULL,
    -- PER-ARM (nulled by ClearRolloutGate, re-stamped on the next indefinite pause)
    ADD COLUMN gate_id                   UUID NULL,
    ADD COLUMN gate_armed_at             TIMESTAMPTZ NULL,
    ADD COLUMN gate_paused_step          INT NULL,
    ADD COLUMN gate_rollout_cluster      TEXT NULL,
    ADD COLUMN gate_rollout_namespace    TEXT NULL,
    ADD COLUMN gate_rollout_name         TEXT NULL,
    -- DECISION
    ADD COLUMN gate_decision             TEXT NULL,
    ADD COLUMN gate_decided_by           TEXT NULL,
    ADD COLUMN gate_decided_at           TIMESTAMPTZ NULL,
    -- ACTION (watcher actuation, not observed)
    ADD COLUMN gate_actioned_at          TIMESTAMPTZ NULL,
    ADD COLUMN rollout_abort_actioned_at TIMESTAMPTZ NULL,
    ADD CONSTRAINT deploy_watches_gate_decision_chk
        CHECK (gate_decision IS NULL OR gate_decision IN ('approved', 'rejected'));

-- +goose StatementEnd
