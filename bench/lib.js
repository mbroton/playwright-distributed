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
