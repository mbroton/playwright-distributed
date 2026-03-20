#!/usr/bin/env node

/**
 * Ensures the Playwright package version declared in worker/package.json
 * matches the Playwright image tag used in worker/Dockerfile.
 */

const { readFileSync } = require('node:fs');
const { resolve } = require('node:path');

const rootDir = resolve(__dirname, '..');
const workerDir = resolve(rootDir, 'worker');

const dockerfilePath = resolve(workerDir, 'Dockerfile');
const packageJsonPath = resolve(workerDir, 'package.json');

function getDockerfileVersion() {
  const dockerfileContent = readFileSync(dockerfilePath, 'utf-8');
  const match = dockerfileContent.match(/FROM\s+mcr\.microsoft\.com\/playwright:v?([\w.-]+)/i);

  if (!match) {
    throw new Error(`Playwright base image tag not found in ${dockerfilePath}`);
  }

  return match[1];
}

function getPackageJsonVersion() {
  const pkg = JSON.parse(readFileSync(packageJsonPath, 'utf-8'));
  const version =
    pkg.dependencies?.['playwright-core'] ??
    pkg.devDependencies?.['playwright-core'] ??
    pkg.peerDependencies?.['playwright-core'] ??
    pkg.dependencies?.playwright ??
    pkg.devDependencies?.playwright ??
    pkg.peerDependencies?.playwright;

  if (!version) {
    throw new Error(`Playwright package dependency not found in ${packageJsonPath}`);
  }

  const versionMatch = String(version).match(/(\d+\.\d+\.\d+)/);

  if (!versionMatch) {
    throw new Error(`Unable to parse Playwright package version "${version}" from ${packageJsonPath}`);
  }

  return versionMatch[1];
}

try {
  const dockerfileVersion = getDockerfileVersion();
  const packageJsonVersion = getPackageJsonVersion();

  if (dockerfileVersion !== packageJsonVersion) {
    console.error(
      `Playwright version mismatch: Dockerfile uses ${dockerfileVersion}, package.json declares ${packageJsonVersion}`
    );
    process.exit(1);
  }

  console.log(`Playwright versions match (${dockerfileVersion}).`);
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
}
