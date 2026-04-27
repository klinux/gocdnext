-- name: ListRunnerProfiles :many
-- Admin UI hot path. Sorted by name so the table reads alphabetical.
SELECT id, name, description, engine,
       default_image,
       default_cpu_request, default_cpu_limit,
       default_mem_request, default_mem_limit,
       max_cpu, max_mem,
       tags, config,
       created_at, updated_at
FROM runner_profiles
ORDER BY name;

-- name: GetRunnerProfile :one
SELECT id, name, description, engine,
       default_image,
       default_cpu_request, default_cpu_limit,
       default_mem_request, default_mem_limit,
       max_cpu, max_mem,
       tags, config,
       created_at, updated_at
FROM runner_profiles
WHERE id = $1
LIMIT 1;

-- name: GetRunnerProfileByName :one
-- Pipeline apply + scheduler dispatch both look up by name (the
-- stable identifier in YAML), not by id, so renames are deliberate
-- breaking changes — admins know what they're doing.
SELECT id, name, description, engine,
       default_image,
       default_cpu_request, default_cpu_limit,
       default_mem_request, default_mem_limit,
       max_cpu, max_mem,
       tags, config,
       created_at, updated_at
FROM runner_profiles
WHERE name = $1
LIMIT 1;

-- name: InsertRunnerProfile :one
INSERT INTO runner_profiles (
    name, description, engine,
    default_image,
    default_cpu_request, default_cpu_limit,
    default_mem_request, default_mem_limit,
    max_cpu, max_mem,
    tags, config
) VALUES (
    $1, $2, $3,
    $4,
    $5, $6,
    $7, $8,
    $9, $10,
    $11, $12
)
RETURNING id, name, description, engine,
          default_image,
          default_cpu_request, default_cpu_limit,
          default_mem_request, default_mem_limit,
          max_cpu, max_mem,
          tags, config,
          created_at, updated_at;

-- name: UpdateRunnerProfile :exec
UPDATE runner_profiles
SET name = $2,
    description = $3,
    engine = $4,
    default_image = $5,
    default_cpu_request = $6, default_cpu_limit = $7,
    default_mem_request = $8, default_mem_limit = $9,
    max_cpu = $10, max_mem = $11,
    tags = $12,
    config = $13,
    updated_at = NOW()
WHERE id = $1;

-- name: DeleteRunnerProfile :exec
-- Caller MUST check no pipeline definition references this profile
-- before issuing the delete; the scheduler resolves profiles by
-- name at dispatch and a missing name fails the run.
DELETE FROM runner_profiles WHERE id = $1;
