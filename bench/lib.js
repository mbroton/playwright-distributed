// Shared helpers for the benchmark scripts.

export function quantile(sortedValues, q) {
  if (sortedValues.length === 0) {
    return NaN;
  }
  const position = (sortedValues.length - 1) * q;
  const lower = Math.floor(position);
  const upper = Math.ceil(position);
  const weight = position - lower;
  return sortedValues[lower] * (1 - weight) + sortedValues[upper] * weight;
}

export function summarize(samples) {
  if (samples.length === 0) {
    throw new Error('summarize() needs at least one sample');
  }
  const sorted = [...samples].sort((a, b) => a - b);
  const sum = sorted.reduce((total, value) => total + value, 0);
  return {
    count: sorted.length,
    min: sorted[0],
    p50: quantile(sorted, 0.5),
    p95: quantile(sorted, 0.95),
    max: sorted[sorted.length - 1],
    mean: sum / sorted.length,
  };
}

export function formatRow(label, stats) {
  const cell = (value) => `${value.toFixed(0)}ms`.padStart(9);
  return (
    label.padEnd(24) +
    cell(stats.min) +
    cell(stats.p50) +
    cell(stats.p95) +
    cell(stats.max) +
    cell(stats.mean)
  );
}

export function formatHeader() {
  const cell = (value) => value.padStart(9);
  return (
    ''.padEnd(24) +
    cell('min') +
    cell('p50') +
    cell('p95') +
    cell('max') +
    cell('mean')
  );
}

// Thrown errors, not process.exit(): an exit() call can truncate output that
// is still buffered in a pipe (node script | tee), losing the very message
// that explains the failure. An uncaught throw is written synchronously and
// still exits 1.
export function parsePositiveInt(name, raw, { min = 1 } = {}) {
  const value = Number(raw);
  if (!Number.isInteger(value) || value < min) {
    throw new Error(`${name} must be an integer >= ${min} (got "${raw}")`);
  }
  return value;
}

export async function withDeadline(promise, ms, label) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${label} exceeded ${ms}ms`)), ms);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

// Polls /v1/capacity (derived from the ws endpoint) until exactly the
// expected number of workers for the benchmarked browser has registered and
// is idle, then verifies they have enough slots for the run. Exact count,
// per browser: leftover workers from a previous stack, or workers of another
// browser type, would silently change what the run measures.
export async function waitForWorkers(wsEndpoint, expectedWorkers, requiredSlots, browser = 'chromium') {
  const url = new URL(wsEndpoint);
  url.protocol = url.protocol === 'wss:' ? 'https:' : 'http:';
  url.pathname = '/v1/capacity';
  // An authenticated grid is benched by putting ?token=... in --endpoint;
  // connect() sends it as-is and the capacity poll turns it into a bearer.
  const token = url.searchParams.get('token');
  url.search = '';
  const headers = token ? { Authorization: `Bearer ${token}` } : {};

  const deadline = Date.now() + 120_000;
  let entry;
  for (;;) {
    try {
      const response = await fetch(url, { headers, signal: AbortSignal.timeout(5_000) });
      if (response.status === 401 || response.status === 403) {
        throw new Error(
          `Capacity check got HTTP ${response.status}: the server requires an API key. ` +
            "Pass it in the endpoint: --endpoint 'ws://host:8080/?token=pwd_...'"
        );
      }
      if (response.ok) {
        const capacity = await response.json();
        entry = capacity.browsers.find((candidate) => candidate.browser === browser);
        if (entry && entry.workers === expectedWorkers && entry.active_sessions === 0) {
          break;
        }
      }
    } catch (error) {
      if (error instanceof Error && error.message.startsWith('Capacity check got HTTP')) {
        throw error;
      }
      // Server may still be starting; keep polling until the deadline.
    }
    if (Date.now() > deadline) {
      throw new Error(
        `Timed out waiting for exactly ${expectedWorkers} idle ${browser} workers` +
          (entry ? ` (last capacity: ${JSON.stringify(entry)})` : '')
      );
    }
    await new Promise((resolve) => setTimeout(resolve, 1_000));
  }
  if (entry.available_slots < requiredSlots) {
    throw new Error(
      `${browser} workers have ${entry.available_slots} slots but the run needs ${requiredSlots}; ` +
        'lower --concurrency or add workers so the result measures service rate, not queueing'
    );
  }
}
