// Playwright resolution for the workbench checks.
//
// The browser checks are OPTIONAL gates: this suite does not depend on a browser
// runtime, and pulling one into `web/package.json` would make every `npm ci`
// download a browser. Instead the driver is borrowed from an existing checkout:
//
//   PLAYWRIGHT_ROOT=<dir with node_modules/@playwright/test>  (default: this
//   repository, then the sibling evoke-ui checkout, then $PWD)
//
// A missing driver is reported as an explicit skip instead of a silent pass.
import { createRequire } from 'node:module';
import { existsSync } from 'node:fs';
import path from 'node:path';

// An explicit root is authoritative: falling back anyway would silently ignore
// the operator's intent (and make "check the skip path" untestable).
const explicit = process.env.PLAYWRIGHT_ROOT || process.env.CUMULUS_PLAYWRIGHT_ROOT;
const CANDIDATES = explicit ? [explicit] : [
  path.resolve(import.meta.dirname, '../..'),
  '/Users/willove/wil-works/evoke-ui-project',
  process.cwd(),
].filter(Boolean);

export function loadPlaywright() {
  const tried = [];
  for (const root of CANDIDATES) {
    const pkg = path.join(root, 'package.json');
    if (!existsSync(pkg)) { tried.push(root + ' (no package.json)'); continue; }
    try {
      const require = createRequire(pkg);
      return require('@playwright/test');
    } catch (error) {
      tried.push(root + ' (' + error.code + ')');
    }
  }
  return { missing: tried };
}

export const PLAYWRIGHT_HINT = 'install @playwright/test in one of the roots above, or set PLAYWRIGHT_ROOT to a checkout that has it (and run `npx playwright install chromium` once)';