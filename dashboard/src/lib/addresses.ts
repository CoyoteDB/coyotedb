// Parsing the HOST:PORT list typed into "Change addresses" (Settings), the
// same rules as POST /v1/members/{node}/addresses: 1 to 8 addresses.
export const MAX_ADDRESSES = 8;

/** Splits on commas, spaces and new lines; drops empties and repeats. */
export function splitAddresses(text: string): string[] {
  const out: string[] = [];
  for (const a of text.split(/[\s,]+/)) if (a && !out.includes(a)) out.push(a);
  return out;
}

/** A problem with one HOST:PORT, in plain words, or null. IPv6 goes in brackets. */
export function addressProblem(a: string): string | null {
  const m = /^(\[[0-9a-fA-F:.]+\]|[^\s:[\]]+):(\d{1,5})$/.exec(a);
  if (!m) return `“${a}” is not HOST:PORT (for example 100.64.0.42:7481).`;
  const port = Number(m[2]);
  if (port < 1 || port > 65535) return `“${a}”: the port must be between 1 and 65535.`;
  return null;
}
