// Measures grid throughput: how many complete browser sessions the grid
// serves per second at a fixed client concurrency.
//
// Each session is connect -> newContext -> newPage -> goto -> close. Keep
// --concurrency at or below the grid's capacity (workers x MAX_SLOTS) so the
// result measures service rate, not admission-control queueing.

import { parseArgs } from 'node:util';
import { chromium } from 'playwright';
import { summarize, formatRow, formatHeader } from './lib.js';

const PAGE_URL = 'data:text/html,<h1>bench</h1>';

const { values: options } = parseArgs({
  options: {
    endpoint: { type: 'string', default: 'ws://127.0.0.1:8080' },
    sessions: { type: 'string', default: '200' },
    concurrency: { type: 'string', default: '5' },
    label: { type: 'string', default: '' },
  },
});

const totalSessions = Number.parseInt(options.sessions, 10);
const concurrency = Number.parseInt(options.concurrency, 10);
if (!Number.isInteger(totalSessions) || totalSessions < 1 || !Number.isInteger(concurrency) || concurrency < 1) {
  console.error('--sessions and --concurrency must be integers >= 1');
  process.exit(1);
}

async function runSession() {
  const start = performance.now();
  const browser = await chromium.connect(options.endpoint);
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    await page.goto(PAGE_URL);
    return performance.now() - start;
  } finally {
    await browser.close();
  }
}

const durations = [];
const errors = [];
let started = 0;

async function clientLoop() {
  while (started < totalSessions) {
    started++;
    try {
      durations.push(await runSession());
    } catch (error) {
      errors.push(error instanceof Error ? error.message : String(error));
    }
  }
}

const benchStart = performance.now();
await Promise.all(Array.from({ length: concurrency }, clientLoop));
const elapsedSeconds = (performance.now() - benchStart) / 1000;

const label = options.label ? ` [${options.label}]` : '';
console.log(
  `throughput${label}: ${totalSessions} sessions, concurrency ${concurrency}\n`
);
console.log(
  `completed ${durations.length}/${totalSessions} in ${elapsedSeconds.toFixed(1)}s ` +
    `= ${(durations.length / elapsedSeconds).toFixed(1)} sessions/s ` +
    `(${((durations.length / elapsedSeconds) * 60).toFixed(0)} sessions/min)`
);
if (durations.length > 0) {
  console.log('\nper-session duration:');
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
