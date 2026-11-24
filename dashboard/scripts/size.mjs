// Reports the gzipped size of dist/ and fails the build above the budget.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { gzipSync } from 'node:zlib';

const BUDGET = 400 * 1024;
const dist = join(import.meta.dirname, '..', 'dist');
const rows = [];
const walk = (d) => {
  for (const f of readdirSync(d)) {
    const p = join(d, f);
    if (statSync(p).isDirectory()) walk(p);
    else if (f !== '.gitkeep') {
      const b = readFileSync(p);
      rows.push({ file: relative(dist, p), raw: b.length, gz: gzipSync(b, { level: 9 }).length });
    }
  }
};
walk(dist);
rows.sort((a, b) => b.gz - a.gz);
const kb = (n) => (n / 1024).toFixed(1).padStart(7) + ' KB';
let raw = 0;
let gz = 0;
for (const r of rows) {
  raw += r.raw;
  gz += r.gz;
  console.log(`${kb(r.raw)} ${kb(r.gz)} gz  ${r.file}`);
}
console.log(`${kb(raw)} ${kb(gz)} gz  total (${rows.length} files, budget ${BUDGET / 1024} KB gz)`);
if (gz > BUDGET) {
  console.error('dist/ is over the size budget.');
  process.exit(1);
}
