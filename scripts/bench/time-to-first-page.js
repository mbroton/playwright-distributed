// Measures time-to-first-page: how long a client waits between asking for a
// browser and having a page ready to use.
//
//   grid:         chromium.connect(<endpoint>) -> newContext -> newPage -> goto
//   local:        chromium.launch()            -> newContext -> newPage -> goto
//   local-reused: newContext -> newPage -> goto on one browser launched
//                 upfront (how @playwright/test reuses a worker's browser)
//
// The three variants bound the comparison: "local" is the cost of a browser
// per task; "local-reused" is the floor a warm local browser sets, and the
// gap between it and "grid" is the grid's relay and scheduling overhead.
// Isolation is context-level in every variant — a worker multiplexes its
// sessions as contexts on one browser process, like a local reused browser
// does. The page is a data: URL, so no network time pollutes the numbers.
// Iterations run sequentially; warmup runs are discarded (they pay one-time
// costs such as module loading and disk cache).

import { parseArgs } from 'node:util';
import { chromium } from 'playwright';
import {
  summarize,
  formatRow,
  formatHeader,
  parsePositiveInt,
  waitForWorkers,
  withDeadline,
} from './lib.js';

const PAGE_URL = 'data:text/html,<h1>bench</h1>';
const connectTimeoutMs = 30_000;
const pageOpsTimeoutMs = 60_000;
const closeTimeoutMs = 15_000;
const MODES = ['all', 'grid', 'local', 'local-reused'];

const { values: options } = parseArgs({
  options: {
    endpoint: { type: 'string', default: 'ws://127.0.0.1:8080' },
    iterations: { type: 'string', default: '30' },
    warmup: { type: 'string', default: '3' },
    'expect-workers': { type: 'string', default: '' },
    mode: { type: 'string', default: 'all' },
  },
});

const iterations = parsePositiveInt('--iterations', options.iterations);
const warmup = parsePositiveInt('--warmup', options.warmup, { min: 0 });
if (!MODES.includes(options.mode)) {
  throw new Error(`--mode must be one of: ${MODES.join(', ')}`);
}
const enabled = (mode) => options.mode === 'all' || options.mode === mode;

if (options['expect-workers'] && enabled('grid')) {
  const expectedWorkers = parsePositiveInt('--expect-workers', options['expect-workers']);
  await waitForWorkers(options.endpoint, expectedWorkers, 1);
}

async function timeToFirstPage(getBrowser, cleanup) {
  const start = performance.now();
  const browser = await getBrowser();
  try {
    // A wedged browser could stall any of these without a deadline.
    await withDeadline(
      (async () => {
        const context = await browser.newContext();
        const page = await context.newPage();
        await page.goto(PAGE_URL);
      })(),
      pageOpsTimeoutMs,
      'page setup'
    );
    return performance.now() - start;
  } finally {
    await cleanup(browser);
  }
}

async function run(label, getBrowser, cleanup) {
  for (let i = 0; i < warmup; i++) {
    await timeToFirstPage(getBrowser, cleanup);
  }
  const samples = [];
  for (let i = 0; i < iterations; i++) {
    samples.push(await timeToFirstPage(getBrowser, cleanup));
  }
  return { label, stats: summarize(samples) };
}

const closeBrowser = (browser) => withDeadline(browser.close(), closeTimeoutMs, 'browser.close()');
const results = [];

if (enabled('grid')) {
  results.push(
    await run(
      'grid connect()',
      () => chromium.connect(options.endpoint, { timeout: connectTimeoutMs }),
      closeBrowser
    )
  );
}
if (enabled('local')) {
  results.push(
    await run('local launch()', () => chromium.launch({ timeout: connectTimeoutMs }), closeBrowser)
  );
}
if (enabled('local-reused')) {
  const shared = await chromium.launch({ timeout: connectTimeoutMs });
  try {
    // The shared browser outlives every iteration; only its contexts close.
    results.push(
      await run(
        'local reused browser',
        () => shared,
        async (browser) => {
          for (const context of browser.contexts()) {
            await withDeadline(context.close(), closeTimeoutMs, 'context.close()');
          }
        }
      )
    );
  } finally {
    await withDeadline(shared.close(), closeTimeoutMs, 'browser.close()');
  }
}

console.log(`time-to-first-page, ${iterations} iterations (${warmup} warmup)\n`);
console.log(formatHeader());
for (const { label, stats } of results) {
  console.log(formatRow(label, stats));
}
