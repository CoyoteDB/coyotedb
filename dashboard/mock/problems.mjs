// Problems with a stable code (dashboard/API.md §5.1) for the mock: the
// same shape and the same kind of words the agent writes
// (internal/agent problems). The agent builds `markdown` the same way.

const ago = (ms) => new Date(Date.now() - ms);
const iso = (d) => new Date(d).toISOString().replace(/\.\d{3}Z$/, 'Z');
const host = (hp) => hp.replace(/:\d+$/, '').replace(/^\[|\]$/g, '');

/** Markdown for an AI agent: self-contained, never any secret. */
export function markdown(p) {
  const out = [`## CoyoteDB problem: \`${p.code}\`${p.node ? ` on ${p.node}` : ''}`, '', p.summary];
  if (p.fix) out.push('', `**What to do:** ${p.fix}`);
  out.push('', '### What happened', '', p.details);
  if (p.evidence?.length) out.push('', '### Evidence', '', ...p.evidence.map((e) => `- ${e}`));
  if (p.commands?.length) out.push('', '### Fix', '', '```sh', ...p.commands, '```');
  out.push('', `Run \`coyote explain ${p.code}\` for what this code means. This text was copied from the CoyoteDB dashboard; it never includes invite codes, passwords or keys.`);
  return out.join('\n');
}

function finish(p) {
  return { ...p, markdown: markdown(p) };
}

/** address_gone: the computer has a new address; the cluster still records the old one. */
export function addressGone(node, was, now, others, sinceMs = 4 * 60_000) {
  const since = ago(sinceMs);
  const w = host(was);
  const n = host(now);
  return finish({
    code: 'address_gone',
    severity: 'problem',
    node,
    summary: `${node}'s address changed: it was ${w}, it is now ${n}. The cluster still records ${w}, so the other computers cannot reach it.`,
    fix: `Tell the cluster ${node}'s new address: press Use ${n}, or run the command below.`,
    details:
      `${node} no longer has ${w}, the address the other computers use to reach it. It has ${n} now. ` +
      `CoyoteDB does not change a recorded address by itself: a computer that answers at a new address could be a different machine, so a person confirms it.\n\n` +
      `Likely causes: the home or office router gave ${node} a new address (DHCP), ${node} moved to another network, or a VPN was turned off. ` +
      `To stop it happening again, reserve the address in your router, or use Tailscale addresses, which do not change.`,
    evidence: [
      `Recorded for ${node}: ${was} (since ${iso(ago(3 * 86_400_000))})`,
      `${node} has now: ${now} (seen at ${iso(since)}, by ${node} itself)`,
      `${others.join(' and ')} cannot connect to ${was}: last contact ${iso(since)}`,
      `log on ${node}: level=WARN msg="this computer no longer has the address the cluster records for it" code=address_gone recorded=${was} now=${now}`,
    ],
    commands: [`coyote node set-address ${node} ${now}`],
    since: iso(since),
    address: { recorded: [was], now: [now], suggested: [now], unreachable_from: others },
  });
}

/** address_unreachable: it reaches the others, they cannot reach it (one way). */
export function addressUnreachable(node, addr, others, sinceMs = 90_000) {
  const since = ago(sinceMs);
  return finish({
    code: 'address_unreachable',
    severity: 'problem',
    node,
    summary: `${others.join(' and ')} cannot connect to ${node} at ${host(addr)} since ${since.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })}. ${node} can still connect to them, so only one direction works.`,
    fix: `Check that ${node} accepts connections on port ${addr.split(':').pop()} (firewall), and that ${host(addr)} is still its address.`,
    details:
      `${node} still has ${host(addr)} and connects out to the others, but nothing can connect in to it. ` +
      `If it is the main node it gives up its lease and another computer takes over, so no two computers act as the main node.\n\n` +
      `Likely causes: a firewall on ${node} or the network blocks incoming connections on the cluster port; a router forwards the port to another machine; or ${node} sits behind NAT and ${host(addr)} is not reachable from outside.`,
    evidence: [
      `Recorded for ${node}: ${addr}`,
      `${node} reaches ${others.join(', ')} (outgoing works)`,
      `${others.join(', ')}: no answer from ${addr} since ${iso(since)}`,
    ],
    commands: [`coyote status --details`],
    since: iso(since),
    address: { recorded: [addr], now: [addr], unreachable_from: others, one_way: true },
  });
}

/** Clears the address problems a saved address fixes (POST /v1/members/{node}/addresses). */
export function afterSetAddress(problems, nodeName, saved) {
  return (problems ?? []).filter((p) => !(p.node === nodeName && p.code === 'address_gone' && (p.address?.suggested ?? []).some((a) => saved.includes(a))));
}

/** Addresses that answer as nodeName in the mock even though the node looks unreachable. */
export function answersAt(problems, nodeName) {
  return (problems ?? []).filter((p) => p.node === nodeName).flatMap((p) => p.address?.suggested ?? []);
}

/** GET /v1/problem-codes: the static catalogue (`coyote explain CODE`). */
export const PROBLEM_CODES = [
  {
    code: 'address_gone',
    title: 'A computer no longer has its recorded address',
    meaning: 'This computer no longer has the address the other computers use to reach it. They cannot connect to it until the cluster records the new one.',
    causes: ['The router gave the computer a new address (DHCP).', 'The computer moved to another network.', 'A VPN or Tailscale was turned off.'],
    fix: 'Record the new address with `coyote node set-address NODE NEWHOST:PORT` (or press Use … in the dashboard). With no leader, run it with --local on each other computer.',
    commands: ['coyote node set-address NODE NEWHOST:PORT', 'coyote node set-address --local NODE NEWHOST:PORT'],
  },
  {
    code: 'address_unreachable',
    title: 'The others cannot connect to a computer',
    meaning: 'A computer connects out to the others, but they cannot connect in to it at its recorded address.',
    causes: ['A firewall blocks the cluster port.', 'NAT or port forwarding points elsewhere.', 'The recorded address is wrong.'],
    fix: 'Open the cluster port (7481 by default) on that computer, or record the right address with `coyote node set-address`.',
    commands: ['coyote status --details', 'coyote node set-address NODE HOST:PORT'],
  },
];
