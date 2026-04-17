-- name: InsertModification :one
INSERT INTO modifications (
    material_id, revision, branch, author, message, payload, committed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (material_id, revision, branch) DO NOTHING
RETURNING id, material_id, revision, branch, author, message, payload, committed_at, detected_at;

-- name: GetModificationByKey :one
SELECT id, material_id, revision, branch, author, message, payload, committed_at, detected_at
FROM modifications
WHERE material_id = $1 AND revision = $2 AND branch IS NOT DISTINCT FROM $3
LIMIT 1;
