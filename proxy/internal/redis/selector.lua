-- Selects the best available worker using a lifetime-first strategy.
-- This prevents multiple workers from reaching their session limit simultaneously
-- by preferring workers with higher lifetime connections, thus staggering restarts.
-- Returns the selected worker UUID or nil if none available.

local max_concurrent_sessions = tonumber(ARGV[1])
local max_lifetime_sessions = tonumber(ARGV[2])
-- Require a valid browser type argument
local browser_type = nil
if ARGV[3] ~= nil and tostring(ARGV[3]) ~= '' then
    browser_type = tostring(ARGV[3])
end

-- Only allow known types; if missing or unknown, treat as no available workers
if browser_type ~= 'chromium' and browser_type ~= 'firefox' and browser_type ~= 'webkit' then
    return nil
end

local prefix = browser_type .. ':'

local active_hash = 'cluster:active_connections'
local lifetime_hash = 'cluster:lifetime_connections'

local active_data = redis.call('HGETALL', active_hash)
local lifetime_data = redis.call('HGETALL', lifetime_hash)
local active_map = {}
local lifetime_map = {}

for i = 1, #active_data, 2 do
    if string.sub(active_data[i], 1, #prefix) == prefix then
        local uuid = active_data[i]
        active_map[uuid] = tonumber(active_data[i+1] or 0)
    end
end
for i = 1, #lifetime_data, 2 do
    if string.sub(lifetime_data[i], 1, #prefix) == prefix then
        local uuid = lifetime_data[i]
        lifetime_map[uuid] = tonumber(lifetime_data[i+1] or 0)
    end
end

if next(active_map) == nil then
    return nil
end
local total_workers = 0
for uuid, active in pairs(active_map) do
    total_workers = total_workers + 1
end

local margin = math.max(1, math.floor(max_lifetime_sessions / total_workers))
local best_uuid = nil
local best_lifetime = -1
local best_active = math.huge
local fallback_uuid = nil
local fallback_lifetime = -1
local fallback_active = math.huge

for uuid, active in pairs(active_map) do
    local worker_key = 'worker:' .. uuid
    -- The worker refreshes this key's TTL with every heartbeat. A persistent key
    -- can be left behind if registration stops between HSET and EXPIRE.
    local worker_ttl = redis.call('TTL', worker_key)
    -- Also fetch worker's browserType to strictly match ARGV[3]
    local worker_fields = redis.call('HMGET', worker_key, 'status', 'browserType')
    local status = worker_fields[1]
    local worker_browser_type = worker_fields[2]
    
    local lifetime = lifetime_map[uuid] or 0

    -- Strictly require browser type match in addition to key prefix
    if worker_ttl > 0 and worker_browser_type == browser_type and status == 'available' and active < max_concurrent_sessions and lifetime < max_lifetime_sessions then
        if lifetime < (max_lifetime_sessions - margin) then
            if lifetime > best_lifetime or (lifetime == best_lifetime and active < best_active) then
                best_uuid = uuid
                best_lifetime = lifetime
                best_active = active
            end
        end
        
        if (lifetime + 1) <= max_lifetime_sessions and (lifetime > fallback_lifetime or (lifetime == fallback_lifetime and active < fallback_active)) then
            fallback_uuid = uuid
            fallback_lifetime = lifetime
            fallback_active = active
        end
    end
end

local selected_uuid = best_uuid or fallback_uuid

if not selected_uuid then
    return nil
end

redis.call('HINCRBY', active_hash, selected_uuid, 1)
redis.call('HINCRBY', lifetime_hash, selected_uuid, 1)

return selected_uuid
