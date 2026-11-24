// Empties dist/ but keeps dist/.gitkeep (git tracks the folder so `go build` works without Node).
import { mkdirSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const dist = join(import.meta.dirname, '..', 'dist');
mkdirSync(dist, { recursive: true });
for (const f of readdirSync(dist)) if (f !== '.gitkeep') rmSync(join(dist, f), { recursive: true, force: true });
writeFileSync(join(dist, '.gitkeep'), '');
