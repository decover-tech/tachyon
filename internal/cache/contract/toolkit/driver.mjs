// Drives @actions/cache for the Tachyon cache contract tests. The Go
// harness copies this file next to an `npm install @actions/cache` and runs:
//
//   node driver.mjs save    <paths> <key>
//   node driver.mjs restore <paths> <key> [restore-key...]
//
// <paths> is newline-separated. Results go to $GITHUB_OUTPUT with the same
// names actions/cache uses (cache-hit, cache-matched-key), so the harness
// reads both clients the same way. Like actions/cache, the toolkit reports
// service failures as ::warning:: lines rather than throwing.
import * as cache from '@actions/cache';
import { appendFileSync } from 'node:fs';

const [op, rawPaths, key, ...restoreKeys] = process.argv.slice(2);
const paths = (rawPaths || '').split('\n').filter(Boolean);
const output = (name, value) =>
  appendFileSync(process.env.GITHUB_OUTPUT, `${name}=${value}\n`);

if (!key || paths.length === 0) {
  console.error('usage: driver.mjs save|restore <paths> <key> [restore-key...]');
  process.exit(2);
}

switch (op) {
  case 'save': {
    const id = await cache.saveCache(paths, key);
    output('cache-id', String(id));
    break;
  }
  case 'restore': {
    const matched = await cache.restoreCache(paths, key, restoreKeys);
    output('cache-hit', String(matched === key));
    output('cache-matched-key', matched ?? '');
    break;
  }
  default:
    console.error(`unknown op ${op}`);
    process.exit(2);
}
