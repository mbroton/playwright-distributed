-- Atomically reaping stale workers.
--
-- KEYS[1]: The hash key for active connections.
-- KEYS[2]: The hash key for allocated sessions.
-- KEYS[3]: The hash key for successful sessions.
-- ARGV: A list of worker IDs to check for staleness.
--
-- For each worker ID in ARGV, this script checks if the corresponding worker
-- heartbeat key ("worker:<worker_id>") exists. If it does not exist, the worker
-- is considered stale, and its entries are removed from all session-counter
-- hashes.
--
-- Returns a list of worker IDs that were successfully reaped.

local reaped_ids = {}
for i, worker_id in ipairs(ARGV) do
  local worker_key = "worker:" .. worker_id
  if redis.call("EXISTS", worker_key) == 0 then
    redis.call("HDEL", KEYS[1], worker_id)
    redis.call("HDEL", KEYS[2], worker_id)
    redis.call("HDEL", KEYS[3], worker_id)
    table.insert(reaped_ids, worker_id)
  end
end
return reaped_ids
