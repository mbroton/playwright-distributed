// Measures time-to-first-page: how long a client waits between asking for a
// browser and having a page ready to use.
//
//   grid:  chromium.connect(<endpoint>) -> newContext -> newPage -> goto
//   local: chromium.launch()            -> newContext -> newPage -> goto
//
// The page is a data: URL, so no network time pollutes the numbers. Iterations
// run sequentially; warmup runs are discarded (they pay one-time costs such as
// module loading and disk cache).

import { parseArgs } from 'node:util';
import { chromium } from 'playwright';
import { summarize, formatRow, formatHeader } from './lib.js';

const PAGE_URL = 'data:text/html,<h1>bench</h1>';

const { values: options } = parseArgs({
  options: {
    endpoint: { type: 'string', default: 'ws://127.0.0.1:8080' },
    iterations: { type: 'string', default: '30' },
    warmup: { type: 'string', default: '3' },
    mode: { type: 'string', default: 'both' }, // both | grid | local
  },
});

const iterations = Number.parseInt(options.iterations, 10);
const warmup = Number.parseInt(options.warmup, 10);
if (!Number.isInteger(iterations) || iterations < 1 || !Number.isInteger(warmup) || warmup < 0) {
  console.error('--iterations must be >= 1 and --warmup must be >= 0');
  process.exit(1);
}
if (!['both', 'grid', 'local'].includes(options.mode)) {
  console.error('--mode must be one of: both, grid, local');
  process.exit(1);
}

async function timeToFirstPage(getBrowser) {
  const start = performance.now();
  const browser = await getBrowser();
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    await page.goto(PAGE_URL);
    return performance.now() - start;
  } finally {
    await browser.close();
  }
}

async function run(label, getBrowser) {
  for (let i = 0; i < warmup; i++) {
    await timeToFirstPage(getBrowser);
  }
  const samples = [];
  for (let i = 0; i < iterations; i++) {
    samples.push(await timeToFirstPage(getBrowser));
  }
  return { label, stats: summarize(samples) };
}

const results = [];
if (options.mode !== 'local') {
  results.push(await run('grid connect()', () => chromium.connect(options.endpoint)));
}
if (options.mode !== 'grid') {
  results.push(await run('local launch()', () => chromium.launch()));
}

console.log(`time-to-first-page, ${iterations} iterations (${warmup} warmup)\n`);
console.log(formatHeader());
for (const { label, stats } of results) {
  console.log(formatRow(label, stats));
}
if (results.length === 2) {
  const [grid, local] = results;
  console.log(`\ngrid p50 is ${(local.stats.p50 / grid.stats.p50).toFixed(1)}x faster than local launch p50`);
}
