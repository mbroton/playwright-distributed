-- name: RegisterWorker :one
INSERT INTO workers (
    id,
    address,
    browser,
    playwright_version,
    max_slots,
    status,
    last_heartbeat
) VALUES (
    $1, $2, $3, $4, $5, $6, now()
)
RETURNING *;

-- name: GetWorker :one
SELECT *
FROM workers
WHERE id = $1;

-- name: UpdateWorkerHeartbeat :one
UPDATE workers
SET last_heartbeat = now(),
    -- A heartbeat can revive stalled workers only because the rescuer may mark
    -- available workers stalled; it must never overwrite drain or shutdown intent.
    status = CASE
        WHEN status = 'stalled' THEN 'available'
        ELSE status
    END
WHERE id = $1
RETURNING *;

-- name: SetWorkerStatus :one
UPDATE workers
SET status = sqlc.arg(status)::worker_status
WHERE id = sqlc.arg(id)
  AND (
      (sqlc.arg(status)::worker_status = 'draining' AND status IN ('available', 'draining'))
      OR (
          sqlc.arg(status)::worker_status = 'shutting_down'
          AND status IN ('available', 'draining', 'stalled', 'shutting_down')
      )
  )
RETURNING *;

-- name: ListWorkers :many
SELECT *
FROM workers
ORDER BY created_at, id;

-- name: DeleteWorker :exec
DELETE FROM workers
WHERE id = $1;

-- name: SelectClaimableWorker :one
SELECT w.*
FROM workers AS w
CROSS JOIN LATERAL (
    SELECT count(*) AS active_count
    FROM sessions AS s
    WHERE s.worker_id = w.id
      AND s.status IN ('pending', 'running')
) AS active
WHERE w.status = 'available'
  AND w.browser = sqlc.arg(browser)
  AND starts_with(w.playwright_version, sqlc.arg(version_prefix)::text)
  AND w.last_heartbeat > now() - sqlc.arg(worker_ttl_microseconds)::bigint * interval '1 microsecond'
  AND (
      sqlc.arg(max_lifetime_sessions)::bigint = 0
      OR w.lifetime_sessions < sqlc.arg(max_lifetime_sessions)::bigint
  )
  AND active.active_count < w.max_slots
  AND w.id != ALL(sqlc.arg(excluded_ids)::uuid[])
-- Prefer the oldest worker, then the least loaded worker, so workers reach
-- the lifetime limit and recycle one at a time instead of as a fleet.
ORDER BY w.lifetime_sessions DESC, active.active_count ASC, w.id
FOR UPDATE OF w SKIP LOCKED
LIMIT 1;

-- name: CountActiveSessionsByWorker :one
SELECT count(*)
FROM sessions
WHERE worker_id = $1
  AND status IN ('pending', 'running');

-- name: IncrementWorkerLifetimeSessions :one
UPDATE workers
SET lifetime_sessions = lifetime_sessions + 1
WHERE id = $1
RETURNING *;

-- name: StallSilentWorkers :execrows
UPDATE workers
SET status = 'stalled'
-- This predicate is load-bearing. The heartbeat can revive stalled workers
-- only because this sweep never overwrites drain or shutdown intent.
WHERE status = 'available'
  AND last_heartbeat < now() - sqlc.arg(worker_ttl_microseconds)::bigint * interval '1 microsecond';

-- name: DeleteDeadWorkers :many
DELETE FROM workers AS w
WHERE (
    (
        w.status = 'shutting_down'
        AND w.last_heartbeat < now() - sqlc.arg(worker_ttl_microseconds)::bigint * interval '1 microsecond'
    ) OR (
        w.status = 'stalled'
        AND w.last_heartbeat < now() - sqlc.arg(stalled_worker_ttl_microseconds)::bigint * interval '1 microsecond'
    ) OR (
        w.status = 'draining'
        AND w.last_heartbeat < now() - sqlc.arg(stalled_worker_ttl_microseconds)::bigint * interval '1 microsecond'
    )
  )
  AND NOT EXISTS (
      SELECT 1
      FROM sessions AS s
      WHERE s.worker_id = w.id
        AND s.status IN ('pending', 'running')
  )
RETURNING w.id;

-- name: GetCapacityByBrowser :many
WITH eligible_workers AS (
    SELECT
        w.browser,
        w.max_slots,
        count(s.id) AS active_count
    FROM workers AS w
    LEFT JOIN sessions AS s
      ON s.worker_id = w.id
     AND s.status IN ('pending', 'running')
    WHERE w.status = 'available'
      AND w.last_heartbeat > now() - sqlc.arg(worker_ttl_microseconds)::bigint * interval '1 microsecond'
      AND (
          sqlc.arg(max_lifetime_sessions)::bigint = 0
          OR w.lifetime_sessions < sqlc.arg(max_lifetime_sessions)::bigint
      )
    GROUP BY w.id
), eligible AS (
    SELECT
        browser,
        count(*) AS workers,
        COALESCE(sum(max_slots), 0)::bigint AS max_slots,
        COALESCE(sum(GREATEST(max_slots::bigint - active_count, 0)), 0)::bigint AS available_slots
    FROM eligible_workers
    GROUP BY browser
), active AS (
    SELECT browser, count(*) AS active_sessions
    FROM sessions
    WHERE status IN ('pending', 'running')
    GROUP BY browser
), browsers AS (
    SELECT browser FROM eligible
    UNION
    SELECT browser FROM active
)
SELECT
    browsers.browser::text AS browser,
    COALESCE(eligible.workers, 0)::bigint AS workers,
    COALESCE(eligible.max_slots, 0)::bigint AS max_slots,
    COALESCE(active.active_sessions, 0)::bigint AS active_sessions,
    COALESCE(eligible.available_slots, 0)::bigint AS available_slots
FROM browsers
LEFT JOIN eligible USING (browser)
LEFT JOIN active USING (browser)
ORDER BY browsers.browser;

-- name: NotifyCapacityChanged :exec
SELECT pg_notify('capacity_changed', '');
