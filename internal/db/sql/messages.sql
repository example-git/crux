-- name: GetMessage :one
SELECT *
FROM messages
WHERE id = ? LIMIT 1;

-- name: ListMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ?
ORDER BY created_at ASC, rowid ASC;

-- name: ListMessagesBySessionFrom :many
SELECT m.*
FROM messages m
JOIN messages checkpoint ON checkpoint.id = sqlc.arg(message_id)
WHERE m.session_id = sqlc.arg(session_id)
  AND checkpoint.session_id = m.session_id
  AND (
    m.created_at > checkpoint.created_at
    OR (
      m.created_at = checkpoint.created_at
      AND m.rowid >= checkpoint.rowid
    )
  )
ORDER BY m.created_at ASC, m.rowid ASC;

-- name: CreateMessage :one
INSERT INTO messages (
    id,
    session_id,
    role,
    parts,
    model,
    provider,
    is_summary_message,
    created_at,
    updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now'), strftime('%s', 'now')
)
RETURNING *;

-- name: UpdateMessage :exec
UPDATE messages
SET
    parts = ?,
    finished_at = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;


-- name: DeleteMessage :exec
DELETE FROM messages
WHERE id = ?;

-- name: DeleteSessionMessages :exec
DELETE FROM messages
WHERE session_id = ?;

-- name: ListUserMessagesBySession :many
SELECT *
FROM messages
WHERE session_id = ? AND role = 'user'
ORDER BY created_at DESC;

-- name: ListAllUserMessages :many
SELECT *
FROM messages
WHERE role = 'user'
ORDER BY created_at DESC;

-- name: GetLastAssistantMessageBySession :one
SELECT *
FROM messages
WHERE session_id = ? AND role = 'assistant' AND is_summary_message = 0
ORDER BY created_at DESC
LIMIT 1;
