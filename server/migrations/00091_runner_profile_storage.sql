-- +goose Up
-- +goose StatementBegin

-- Runner profile gains OPTIONAL per-profile storage sizing for the
-- Kubernetes isolated-mode job pod:
--
--   workspace_size / workspace_storage_class
--     override the agent-global workspace ephemeral-PVC sizing
--     (GOCDNEXT_AGENT_WORKSPACE_SIZE / _STORAGE_CLASS) for jobs on
--     this profile. A heavy-build profile can ask for a larger/faster
--     workspace disk without inflating every job.
--
--   dind_storage_size / dind_storage_class
--     when set (and the job runs docker:true), give the DinD sidecar a
--     DEDICATED ephemeral PVC mounted at /var/lib/docker. dockerd +
--     buildkit otherwise store image layers and the export/push
--     staging area on the node's ephemeral disk, whose throughput
--     caps big-image builds. A large premium-rwo (GCE PD throughput
--     scales with size) or a local-SSD class moves that I/O off the
--     bottleneck.
--
-- All four are plain strings carrying k8s quantity / storage-class
-- names. Empty string = "not set" → the agent falls back to its own
-- config (workspace) or keeps the node-disk default (dind). TEXT NOT
-- NULL DEFAULT '' keeps every read path uniform (no NULL handling in
-- the store, scheduler, agent, or UI). The admin API validates the
-- quantity format before it ever reaches a PodSpec.

ALTER TABLE runner_profiles
    ADD COLUMN workspace_size          TEXT NOT NULL DEFAULT '',
    ADD COLUMN workspace_storage_class TEXT NOT NULL DEFAULT '',
    ADD COLUMN dind_storage_size       TEXT NOT NULL DEFAULT '',
    ADD COLUMN dind_storage_class      TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE runner_profiles
    DROP COLUMN IF EXISTS workspace_size,
    DROP COLUMN IF EXISTS workspace_storage_class,
    DROP COLUMN IF EXISTS dind_storage_size,
    DROP COLUMN IF EXISTS dind_storage_class;

-- +goose StatementEnd
