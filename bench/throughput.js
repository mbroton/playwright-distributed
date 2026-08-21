// Measures grid throughput: how many complete browser sessions the grid
// serves per second at a fixed client concurrency.
//
// A session is connect -> newContext -> newPage -> goto -> close, and the
// per-session timer covers all of it, including close. A discarded warmup
// phase runs first so ramp costs (cold client, first context on each worker,
// the scheduler filling slots) stay out of the measured window. Keep
// --concurrency at or below the grid's capacity (workers x MAX_SLOTS) so the
// result measures service rate, not admission-control queueing;
// --expect-workers enforces that before the run starts.

import { parseArgs } from 'node:util';
import { chromium } from 'playwright';
import {
  summarize,
  formatRow,
  formatHeader,
  parsePositiveInt,
  withDeadline,
  waitForWorkers,
} from './lib.js';

const PAGE_URL = 'data:text/html,<h1>bench</h1>';
const connectTimeoutMs = 30_000;
const closeTimeoutMs = 15_000;

const { values: options } = parseArgs({
  options: {
    endpoint: { type: 'string', default: 'ws://127.0.0.1:8080' },
    sessions: { type: 'string', default: '500' },
    concurrency: { type: 'string', default: '5' },
    warmup: { type: 'string', default: '' }, // sessions; default 2 x concurrency
    'expect-workers': { type: 'string', default: '' },
    label: { type: 'string', default: '' },
  },
});

const totalSessions = parsePositiveInt('--sessions', options.sessions);
const concurrency = parsePositiveInt('--concurrency', options.concurrency);
const warmupSessions = options.warmup
  ? parsePositiveInt('--warmup', options.warmup, { min: 0 })
  : 2 * concurrency;

if (options['expect-workers']) {
  const expectedWorkers = parsePositiveInt('--expect-workers', options['expect-workers']);
  await waitForWorkers(options.endpoint, expectedWorkers, concurrency);
}

async function runSession() {
  const start = performance.now();
  const browser = await chromium.connect(options.endpoint, { timeout: connectTimeoutMs });
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    await page.goto(PAGE_URL);
  } finally {
    await withDeadline(browser.close(), closeTimeoutMs, 'browser.close()');
  }
  return performance.now() - start;
}

async function runPhase(sessions, { record }) {
  const durations = [];
  const errors = [];
  let started = 0;
  let completed = 0;

  async function clientLoop() {
    while (started < sessions) {
      started++;
      try {
        durations.push(await runSession());
      } catch (error) {
        errors.push(error instanceof Error ? error.message : String(error));
      }
      completed++;
      if (record && completed % 100 === 0) {
        process.stderr.write(`  ${completed}/${sessions}\n`);
      }
    }
  }

  const phaseStart = performance.now();
  await Promise.all(Array.from({ length: Math.min(concurrency, sessions) }, clientLoop));
  return { durations, errors, elapsedSeconds: (performance.now() - phaseStart) / 1000 };
}

if (warmupSessions > 0) {
  const warmup = await runPhase(warmupSessions, { record: false });
  if (warmup.errors.length > 0) {
    console.error(`${warmup.errors.length}/${warmupSessions} warmup sessions failed; not measuring a broken grid`);
    process.exit(1);
  }
}

const { durations, errors, elapsedSeconds } = await runPhase(totalSessions, { record: true });

const label = options.label ? ` [${options.label}]` : '';
console.log(
  `throughput${label}: ${totalSessions} sessions (after ${warmupSessions} warmup), ` +
    `concurrency ${Math.min(concurrency, totalSessions)}\n`
);
console.log(
  `completed ${durations.length}/${totalSessions} in ${elapsedSeconds.toFixed(1)}s ` +
    `= ${(durations.length / elapsedSeconds).toFixed(1)} sessions/s ` +
    `(${((durations.length / elapsedSeconds) * 60).toFixed(0)} sessions/min)`
);
if (durations.length > 0) {
  console.log('\nper-session duration (connect through close):');
  console.log(formatHeader());
  console.log(formatRow('session', summarize(durations)));
}
if (errors.length > 0) {
  const counts = new Map();
  for (const message of errors) {
    counts.set(message, (counts.get(message) ?? 0) + 1);
  }
  console.error(`\n${errors.length} sessions failed:`);
  for (const [message, count] of counts) {
    console.error(`  ${count}x ${message}`);
  }
  process.exit(1);
}
