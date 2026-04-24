-- name: ListGroups :many
-- UI: admin-only list of groups + a cheap member count so the
-- settings page can render "SRE (4 members)" without a second
-- query per group.
SELECT g.id, g.name, g.description, g.created_by, g.created_at, g.updated_at,
       (SELECT COUNT(*) FROM group_members m WHERE m.group_id = g.id)::INT AS member_count
FROM groups g
ORDER BY g.name;

-- name: GetGroup :one
SELECT id, name, description, created_by, created_at, updated_at
FROM groups
WHERE id = $1
LIMIT 1;

-- name: GetGroupByName :one
-- Used on approve: gate carries group NAMES (not ids) so renames
-- propagate cleanly. Lookup translates name → id → members.
SELECT id, name, description, created_by, created_at, updated_at
FROM groups
WHERE name = $1
LIMIT 1;

-- name: InsertGroup :one
INSERT INTO groups (name, description, created_by)
VALUES ($1, $2, $3)
RETURNING id, name, description, created_by, created_at, updated_at;

-- name: UpdateGroup :exec
UPDATE groups
SET name = $2, description = $3, updated_at = NOW()
WHERE id = $1;

-- name: DeleteGroup :exec
DELETE FROM groups WHERE id = $1;

-- name: ListGroupMembers :many
-- Join against users so the UI renders name/email without a
-- second round-trip. Orders by the user's display name so the
-- admin UI reads alphabetically.
SELECT u.id, u.email, u.name, u.role, gm.added_at
FROM group_members gm
JOIN users u ON u.id = gm.user_id
WHERE gm.group_id = $1
ORDER BY COALESCE(NULLIF(u.name, ''), u.email);

-- name: AddGroupMember :exec
-- Idempotent on (group_id, user_id) — re-adding is a no-op, not
-- an error, so the UI's "add if not present" flow doesn't need
-- a pre-check query.
INSERT INTO group_members (group_id, user_id, added_by)
VALUES ($1, $2, $3)
ON CONFLICT (group_id, user_id) DO NOTHING;

-- name: RemoveGroupMember :exec
DELETE FROM group_members
WHERE group_id = $1 AND user_id = $2;

-- name: ListUserGroupNames :many
-- Permission check hot path: "is this user in any group that's
-- in the gate's approver_groups?" — we fetch the user's group
-- names once per approve call and intersect in Go. Names (not
-- ids) match what the gate stores.
SELECT g.name
FROM group_members gm
JOIN groups g ON g.id = gm.group_id
WHERE gm.user_id = $1;

-- name: InsertJobRunApproval :one
-- Records one vote on a gate. Unique (job_run_id, user_id) —
-- double-voting is a conflict, not a silent dup. Returns
-- (id, created) so the caller sees whether the vote actually
-- landed or was a re-post from the same user.
INSERT INTO job_run_approvals
    (job_run_id, user_id, user_label, decision, comment)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (job_run_id, user_id) DO NOTHING
RETURNING job_run_id, user_id, decision, decided_at;

-- name: CountJobRunApprovals :one
-- Quorum check: how many approved votes (excluding rejects)
-- has this gate accumulated?
SELECT COUNT(*)::INT
FROM job_run_approvals
WHERE job_run_id = $1 AND decision = 'approved';

-- name: ListJobRunApprovals :many
-- Detail trail for the UI: every vote in chronological order.
SELECT user_id, user_label, decision, comment, decided_at
FROM job_run_approvals
WHERE job_run_id = $1
ORDER BY decided_at ASC;
