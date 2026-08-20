-- TEST ONLY: this bypasses the worker lock and capacity invariant. Production
-- session inserts must use InsertClaimedSession while holding the worker row lock.
-- name: InsertSession :one
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
    keep_alive_ms,
    connect_metadata
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6,
    $7,
    $8,
    $9,
    now(),
    $10,
    COALESCE(sqlc.narg(connect_metadata)::jsonb, '{}'::jsonb)
)
RETURNING *;

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
WHERE status IN ('pending', 'running')
  AND (
      last_heartbeat < now() - sqlc.arg(session_ttl_microseconds)::bigint * interval '1 microsecond'
      OR (expires_at IS NOT NULL AND expires_at < now())
  )
RETURNING id, worker_id;

-- name: FailUnreportedWorkerSessions :many
UPDATE sessions
SET status = 'failed'
WHERE worker_id = sqlc.arg(worker_id)
  AND status = 'running'
  AND started_at IS NOT NULL
  AND started_at < now() - sqlc.arg(grace_microseconds)::bigint * interval '1 microsecond'
  AND id != ALL(sqlc.arg(active_session_ids)::uuid[])
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
