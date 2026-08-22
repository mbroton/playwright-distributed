// Head-to-head task benchmark: identical task against playwright-distributed
// and Browserless. Run from a checkout's bench/ directory (resolves the
// pinned playwright there). One (endpoint, task, concurrency) point per
// invocation; the runbook loops over points and repeats.
//
// Tasks:
//   tiny   = connect -> context -> page -> goto (no delay) -> read #x -> close
//   medium = connect -> context -> page -> 3x goto (?delay=300) -> click #btn
//            -> wait #out -> fill #inp -> read -> close   (~1.2s of page work)
import { parseArgs } from 'node:util';
import { chromium } from 'playwright';
import { summarize, formatRow, formatHeader, withDeadline } from './lib.js';

const { values: options } = parseArgs({
  options: {
    endpoint: { type: 'string' },
    target: { type: 'string' }, // e.g. http://172.31.1.56:9999
    task: { type: 'string', default: 'tiny' }, // tiny | medium
    label: { type: 'string', default: '' },
    tasks: { type: 'string', default: '200' },
    concurrency: { type: 'string', default: '5' },
    warmup: { type: 'string', default: '' }, // default 2x concurrency
  },
});
if (!options.endpoint || !options.target) {
  throw new Error('--endpoint and --target are required');
}
if (!['tiny', 'medium'].includes(options.task)) {
  throw new Error('--task must be tiny or medium');
}

async function taskBody(page) {
  if (options.task === 'tiny') {
    await page.goto(`${options.target}/?delay=0`);
    const text = await page.textContent('#x');
    if (text !== 'hello-bench') throw new Error(`unexpected content: ${text}`);
    return;
  }
  await page.goto(`${options.target}/a?delay=300`);
  await page.goto(`${options.target}/b?delay=300`);
  await page.goto(`${options.target}/c?delay=300`);
  await page.click('#btn');
  await page.waitForSelector('#out:has-text("clicked")', { timeout: 10_000 });
  await page.fill('#inp', 'bench');
  const text = await page.textContent('#x');
  if (text !== 'hello-bench') throw new Error(`unexpected content: ${text}`);
}

async function runTask() {
  const start = performance.now();
  const browser = await chromium.connect(options.endpoint, { timeout: 60_000 });
  try {
    await withDeadline(
      (async () => {
        const context = await browser.newContext();
        const page = await context.newPage();
        await taskBody(page);
      })(),
      120_000,
      'task body'
    );
  } finally {
    await withDeadline(browser.close(), 30_000, 'browser.close()');
  }
  return performance.now() - start;
}

const totalTasks = Number(options.tasks);
const concurrency = Number(options.concurrency);
const warmupTasks = options.warmup === '' ? 2 * concurrency : Number(options.warmup);

async function runPhase(count) {
  const durations = [];
  const errors = [];
  let started = 0;
  async function loop() {
    while (started < count) {
      started++;
      try {
        durations.push(await runTask());
      } catch (error) {
        errors.push(error instanceof Error ? error.message : String(error));
      }
    }
  }
  const t0 = performance.now();
  await Promise.all(Array.from({ length: Math.min(concurrency, count) }, loop));
  return { durations, errors, seconds: (performance.now() - t0) / 1000 };
}

if (warmupTasks > 0) {
  const warmup = await runPhase(warmupTasks);
  if (warmup.errors.length > 0) {
    throw new Error(`${warmup.errors.length}/${warmupTasks} warmup tasks failed: ${warmup.errors[0]}`);
  }
}

const { durations, errors, seconds } = await runPhase(totalTasks);
console.log(
  `RESULT label=${options.label} task=${options.task} tasks=${durations.length}/${totalTasks} ` +
    `concurrency=${concurrency} wall=${seconds.toFixed(1)}s rate=${(durations.length / seconds).toFixed(2)}/s`
);
console.log(formatHeader());
console.log(formatRow('task duration', summarize(durations)));
if (errors.length > 0) {
  const counts = new Map();
  for (const message of errors) counts.set(message, (counts.get(message) ?? 0) + 1);
  console.error(`${errors.length} tasks failed:`);
  for (const [message, count] of counts) console.error(`  ${count}x ${message}`);
  process.exitCode = 1;
}
