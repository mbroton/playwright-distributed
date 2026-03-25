-- Selects an eligible worker with balanced routing and an allocated-session
-- bias. The selector stays deterministic: prefer lower active load first, then
-- use allocated headroom to stagger restarts among otherwise similar healthy
-- workers.
--
-- ARGV[1]: MAX_CONCURRENT_SESSIONS
-- ARGV[2]: MAX_LIFETIME_SESSIONS
-- ARGV[3]: browser type
-- ARGV[4]: selector freshness timeout in seconds
-- ARGV[5...]: worker IDs to exclude for this connection attempt
--
-- Returns selected worker metadata as:
-- [id, browserType, endpoint, status, startedAt, lastHeartbeat]
-- or nil if no worker is eligible.

local max_concurrent_sessions = tonumber(ARGV[1])
local max_lifetime_sessions = tonumber(ARGV[2])
local browser_type = nil
if ARGV[3] ~= nil and tostring(ARGV[3]) ~= '' then
    browser_type = tostring(ARGV[3])
end
local freshness_timeout_seconds = tonumber(ARGV[4])

if browser_type == nil or freshness_timeout_seconds == nil or freshness_timeout_seconds <= 0 then
    return nil
end

local excluded_worker_ids = {}
for i = 5, #ARGV do
    local excluded_worker_id = tostring(ARGV[i])
    if excluded_worker_id ~= '' then
        excluded_worker_ids[excluded_worker_id] = true
    end
end

local prefix = browser_type .. ':'
local active_hash = 'cluster:active_connections'
local allocated_hash = 'cluster:allocated_sessions'

local time = redis.call('TIME')
local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
local threshold = now_ms - (freshness_timeout_seconds * 1000)

local allocated_data = redis.call('HGETALL', allocated_hash)
local allocated_map = {}
for i = 1, #allocated_data, 2 do
    local worker_id = allocated_data[i]
    if string.sub(worker_id, 1, #prefix) == prefix then
        allocated_map[worker_id] = tonumber(allocated_data[i + 1] or 0)
    end
end

local function candidate_from_worker(worker_id, active)
    local worker_key = 'worker:' .. worker_id
    local worker_fields = redis.call('HMGET', worker_key, 'id', 'browserType', 'endpoint', 'status', 'startedAt', 'lastHeartbeat')
    local id = worker_fields[1]
    local worker_browser_type = worker_fields[2]
    local endpoint = worker_fields[3]
    local status = worker_fields[4]
    local started_at_str = worker_fields[5]
    local last_heartbeat_str = worker_fields[6]

    if id == false or worker_browser_type == false or endpoint == false or status == false or started_at_str == false or last_heartbeat_str == false then
        return nil
    end

    local started_at = tonumber(started_at_str)
    local last_heartbeat = tonumber(last_heartbeat_str)
    local allocated = allocated_map[worker_id] or 0
    if started_at == nil or last_heartbeat == nil then
        return nil
    end

    if worker_browser_type ~= browser_type or status ~= 'available' then
        return nil
    end

    if active >= max_concurrent_sessions or allocated >= max_lifetime_sessions or last_heartbeat < threshold then
        return nil
    end

    return {
        worker_id = worker_id,
        id = tostring(id),
        browser_type = tostring(worker_browser_type),
        endpoint = tostring(endpoint),
        status = tostring(status),
        started_at = started_at,
        started_at_str = tostring(started_at_str),
        last_heartbeat_str = tostring(last_heartbeat_str),
        active = active,
        allocated = allocated
    }
end

local function better_candidate(left, right)
    if right == nil then
        return true
    end

    if left.active ~= right.active then
        return left.active < right.active
    end

    if left.allocated ~= right.allocated then
        return left.allocated > right.allocated
    end

    if left.started_at ~= right.started_at then
        return left.started_at < right.started_at
    end

    return left.id < right.id
end

local active_data = redis.call('HGETALL', active_hash)
local eligible_workers = {}

for i = 1, #active_data, 2 do
    local worker_id = active_data[i]
    if string.sub(worker_id, 1, #prefix) == prefix and not excluded_worker_ids[worker_id] then
        local active = tonumber(active_data[i + 1] or 0)
        local candidate = candidate_from_worker(worker_id, active)
        if candidate ~= nil then
            table.insert(eligible_workers, candidate)
        end
    end
end

if #eligible_workers == 0 then
    return nil
end

local margin = math.max(1, math.floor(max_lifetime_sessions / #eligible_workers))
local primary_candidate = nil
local fallback_candidate = nil

for _, candidate in ipairs(eligible_workers) do
    if candidate.allocated < (max_lifetime_sessions - margin) then
        if better_candidate(candidate, primary_candidate) then
            primary_candidate = candidate
        end
    elseif (candidate.allocated + 1) <= max_lifetime_sessions then
        if better_candidate(candidate, fallback_candidate) then
            fallback_candidate = candidate
        end
    end
end

local selected = primary_candidate or fallback_candidate
if selected == nil then
    return nil
end

redis.call('HINCRBY', active_hash, selected.worker_id, 1)
redis.call('HINCRBY', allocated_hash, selected.worker_id, 1)

return {
    selected.id,
    selected.browser_type,
    selected.endpoint,
    selected.status,
    selected.started_at_str,
    selected.last_heartbeat_str
}
