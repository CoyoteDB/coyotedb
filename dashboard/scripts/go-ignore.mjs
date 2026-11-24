// Some npm packages ship stray .go files (flatted does). A go.mod inside
// node_modules makes it a separate Go module, so `go vet ./...` and
// `go test ./...` at the repo root never descend into it.
import { existsSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const dir = join(import.meta.dirname, '..', 'node_modules');
if (existsSync(dir)) {
  writeFileSync(join(dir, 'go.mod'), 'module ignored.invalid/node_modules\n\ngo 1.22\n');
}
