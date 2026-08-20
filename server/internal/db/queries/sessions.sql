-- name: GetSession :one
SELECT *
FROM sessions
WHERE id = $1;

-- StartSession is intentionally unused until the PR 4 relay attaches sessions.
-- name: StartSession :one
UPDATE sessions
SET status = 'running',
    started_at = now(),
    last_heartbeat = now(),
    expires_at = NULL
WHERE id = $1
  AND status = 'pending'
RETURNING *;

-- name: CompleteSession :one
WITH updated AS (
    UPDATE sessions
    SET status = 'completed'
    WHERE id = $1
      AND status = 'running'
    RETURNING sessions.*
), notified AS (
    SELECT pg_notify('capacity_changed', '')
    FROM updated
)
SELECT updated.*
FROM updated
CROSS JOIN notified;

-- name: FailSession :one
WITH updated AS (
    UPDATE sessions
    SET status = 'failed'
    WHERE id = $1
      AND status IN ('pending', 'running')
    RETURNING sessions.*
), notified AS (
    SELECT pg_notify('capacity_changed', '')
    FROM updated
)
SELECT updated.*
FROM updated
CROSS JOIN notified;

-- name: RenewSessionHeartbeat :one
UPDATE sessions
SET last_heartbeat = now()
WHERE id = $1
  AND status = 'running'
RETURNING *;

-- name: CountRunningSessionsByWorker :one
SELECT count(*)
FROM sessions
WHERE worker_id = $1
  AND status = 'running';

-- name: InsertClaimedSession :one
INSERT INTO sessions (
    id,
    worker_id,
    browser,
    playwright_version,
    worker_address,
    mode,
    status,
    created_by_key,
    expires_at,
    last_heartbeat,
    connect_metadata
) VALUES (
    sqlc.arg(id),
    sqlc.arg(worker_id),
    sqlc.arg(browser),
    sqlc.arg(playwright_version),
    sqlc.arg(worker_address),
    'default',
    'pending',
    sqlc.narg(created_by_key),
    now() + sqlc.arg(pending_ttl_microseconds)::bigint * interval '1 microsecond',
    now(),
    COALESCE(sqlc.narg(connect_metadata)::jsonb, '{}'::jsonb)
)
RETURNING *;

-- name: ExpireDeadSessions :many
UPDATE sessions
SET status = 'expired'
WHERE id IN (
    -- All bulk session updates lock rows in id order to prevent deadlocks.
    SELECT id
    FROM sessions
    WHERE status IN ('pending', 'running')
      AND (
          last_heartbeat < now() - sqlc.arg(session_ttl_microseconds)::bigint * interval '1 microsecond'
          OR (expires_at IS NOT NULL AND expires_at < now())
      )
    ORDER BY id
    FOR UPDATE
)
RETURNING id, worker_id;

-- name: FailUnreportedWorkerSessions :many
UPDATE sessions
SET status = 'failed'
WHERE id IN (
    SELECT candidate.id
    FROM sessions AS candidate
    WHERE candidate.worker_id = sqlc.arg(worker_id)
      AND candidate.status = 'running'
      AND candidate.started_at IS NOT NULL
      AND candidate.started_at < now() - sqlc.arg(grace_microseconds)::bigint * interval '1 microsecond'
      AND candidate.id != ALL(sqlc.arg(active_session_ids)::uuid[])
    ORDER BY candidate.id
    FOR UPDATE
)
RETURNING id;

-- name: ListStaleWorkerSessionIDs :many
SELECT reported_id::uuid
FROM unnest(sqlc.arg(active_session_ids)::uuid[]) AS reported(reported_id)
EXCEPT
SELECT id
FROM sessions
WHERE worker_id = sqlc.arg(worker_id)
  AND status IN ('pending', 'running')
ORDER BY 1;

-- name: ListSessionsByWorker :many
SELECT *
FROM sessions
WHERE worker_id = $1
ORDER BY created_at, id
LIMIT sqlc.arg(page_size) OFFSET sqlc.arg(page_offset);
