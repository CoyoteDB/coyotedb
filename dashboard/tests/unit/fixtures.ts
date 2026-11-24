// Status fixtures straight from the mock's scenario presets (mock/world.mjs),
// so the unit tests and the end-to-end tests describe the same situations.
import type { Status } from '../../src/api/types';
import { SCENARIOS, status } from '../../mock/world.mjs';

export function scenarioStatus(id: string): Status {
  const s = (SCENARIOS as { id: string; make: () => unknown }[]).find((x) => x.id === id);
  if (!s) throw new Error('no scenario ' + id);
  return status(s.make()) as Status;
}

export function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T;
}
