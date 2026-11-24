// The cluster at a glance, in the site's visual language: discs on a
// horizon, the main node in oxblood with call arcs, copies in ink, the
// witness as a dashed ring. A node that is down is a dashed red outline with
// a cross and the words "not responding" — never colour alone.
import type { HealthSummary, NodeSummary } from '../lib/health';

const R = { data: 22, witness: 14 };
const CY = 96;
const SLOT = 150;

function callArc(cx: number, cy: number, r: number, from = 145, to = 35): string {
  const pt = (deg: number) => {
    const a = (deg * Math.PI) / 180;
    return `${(cx + r * Math.cos(a)).toFixed(2)} ${(cy - r * Math.sin(a)).toFixed(2)}`;
  };
  return `M${pt(from)} A${r} ${r} 0 0 1 ${pt(to)}`;
}

/** Main node in the middle, the others alternating around it. */
export function arrange(nodes: NodeSummary[]): NodeSummary[] {
  const main = nodes.find((n) => n.kind === 'main');
  const rest = nodes.filter((n) => n !== main).sort((a, b) => (a.kind === 'witness' ? 1 : 0) - (b.kind === 'witness' ? 1 : 0));
  if (!main) return rest;
  if (rest.length <= 1) return [main, ...rest];
  const left: NodeSummary[] = [];
  const right: NodeSummary[] = [];
  rest.forEach((n, i) => (i % 2 === 0 ? left : right).push(n));
  return [...left.reverse(), main, ...right];
}

function roleWord(n: NodeSummary): string {
  if (n.condition === 'down') return 'Not responding';
  if (n.condition === 'attention') return 'Needs attention';
  if (n.condition === 'copying') return n.progress != null ? `Copying ${n.progress}%` : 'Copying';
  if (n.condition === 'rejoining') return 'Catching up';
  if (n.condition === 'needs_reclone') return 'Needs fresh copy';
  if (n.condition === 'behind') return 'Not following';
  return n.kind === 'main' ? 'Main node' : n.kind === 'copy' ? 'Copy' : 'Witness';
}

export function NodeDiagram({ summary }: { summary: HealthSummary }) {
  const nodes = arrange(summary.nodes);
  const W = Math.max(380, nodes.length * SLOT);
  const H = 176;
  const pad = (W - nodes.length * SLOT) / 2;
  const x = (i: number) => pad + SLOT * i + SLOT / 2;
  const mainIdx = nodes.findIndex((n) => n.kind === 'main');
  const main = nodes[mainIdx];
  const mainDown = main?.condition === 'down';
  const label = nodes
    .map((n) => `${n.name}: ${roleWord(n).toLowerCase()}${n.isThis ? ' (this computer)' : ''}`)
    .join('; ');

  return (
    <figure class="diagram" style={{ margin: 0 }}>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={`Cluster diagram. ${label}.`}>
        <line class="horizon" x1="8" x2={W - 8} y1={CY + R.data} y2={CY + R.data} />
        {main &&
          nodes.map((n, i) => {
            if (n === main) return null;
            const r1 = R.data;
            const r2 = n.kind === 'witness' ? R.witness : R.data;
            // Links leave the disc low on its side (20° up), under the call arcs.
            const dir = i > mainIdx ? 1 : -1;
            const a = (20 * Math.PI) / 180;
            const x1 = x(mainIdx) + dir * r1 * Math.cos(a);
            const x2 = x(i) - dir * r2 * Math.cos(a);
            const y1 = CY - r1 * Math.sin(a);
            const y2 = CY + (r1 - r2) - r2 * Math.sin(a);
            const rise = Math.abs(x2 - x1) * 0.22 + 4;
            const cx = (x1 + x2) / 2;
            const cy = Math.min(y1, y2) - rise;
            const lost = n.condition === 'down' || n.condition === 'attention' || main.condition === 'attention' || mainDown || n.condition === 'behind' || n.condition === 'needs_reclone';
            const cls =
              n.kind === 'witness'
                ? lost ? 'link lost' : 'link'
                : lost ? 'link lost' : n.condition === 'copying' || n.condition === 'rejoining' ? 'link copying' : n.inSync ? 'link sync live' : 'link live';
            return (
              <path
                key={`l-${n.id}`}
                class={cls}
                d={`M${x1} ${y1} Q${cx} ${cy} ${x2} ${y2}`}
                opacity={n.kind === 'witness' && !lost ? 0.55 : 1}
                stroke-dasharray={n.kind === 'witness' && !lost ? '1.5 4' : undefined}
              />
            );
          })}
        {nodes.map((n, i) => {
          const r = n.kind === 'witness' ? R.witness : R.data;
          const cy = CY + (R.data - r);
          const down = n.condition === 'down';
          const trouble = n.condition === 'attention';
          const copying = n.condition === 'copying' || n.condition === 'rejoining';
          const circ = 2 * Math.PI * (r + 5);
          return (
            <g key={n.id} class="node" style={{ transform: `translate(${x(i)}px, ${cy}px)` }}>
              {n.kind === 'main' && !down && (
                <>
                  <path class="arc a1" d={callArc(0, 0, r + 9)} />
                  <path class="arc a2" d={callArc(0, 0, r + 18)} />
                </>
              )}
              {down ? (
                <>
                  <circle class="node-disc down" r={r} />
                  <path class="node-x" d={`M${-r * 0.36} ${-r * 0.36} L${r * 0.36} ${r * 0.36} M${r * 0.36} ${-r * 0.36} L${-r * 0.36} ${r * 0.36}`} />
                </>
              ) : n.kind === 'witness' ? (
                <circle class="node-ring witness" r={r} />
              ) : copying ? (
                <>
                  <circle class="node-disc copying" r={r} />
                  <circle
                    class="progress-arc"
                    r={r + 5}
                    stroke-dasharray={`${circ} ${circ}`}
                    stroke-dashoffset={circ * (1 - (n.progress ?? 15) / 100)}
                    transform="rotate(-90)"
                  />
                </>
              ) : (
                <circle class={`node-disc ${n.kind === 'main' ? 'main' : 'copy'}`} r={r} />
              )}
              {trouble && (
                <>
                  <circle class="trouble-ring" r={r + 4} />
                  <path class="node-bang" d={`M0 ${-r * 0.42} V${r * 0.12} M0 ${r * 0.36} v0.01`} />
                </>
              )}
              {n.isThis && !down && <circle class="this-ring" r={r + (copying || trouble ? 10 : 4)} stroke-dasharray="1 3" />}
              <text y={r + 24} text-anchor="middle">
                {n.name.length > 16 ? n.name.slice(0, 15) + '…' : n.name}
              </text>
              <text class={`role${down || trouble ? ' down' : ''}`} y={r + 42} text-anchor="middle">
                {roleWord(n)}
              </text>
            </g>
          );
        })}
      </svg>
      <ul class="diagram-legend" aria-hidden="true">
        <li>
          <svg viewBox="0 0 14 14"><circle cx="7" cy="7" r="6" fill="var(--accent)" /></svg>Main node
        </li>
        <li>
          <svg viewBox="0 0 14 14"><circle cx="7" cy="7" r="6" fill="var(--ink)" /></svg>Copy
        </li>
        {summary.witnessCount > 0 && (
          <li>
            <svg viewBox="0 0 14 14"><circle cx="7" cy="7" r="5.5" fill="none" stroke="var(--ink-2)" stroke-dasharray="2 1.6" /></svg>Witness
          </li>
        )}
        {nodes.some((n) => n.condition === 'down') && (
          <li>
            <svg viewBox="0 0 14 14"><circle cx="7" cy="7" r="5.5" fill="none" stroke="var(--down)" stroke-dasharray="2.4 1.8" /><path d="M5 5l4 4M9 5l-4 4" stroke="var(--down)" /></svg>Not responding
          </li>
        )}
        {nodes.some((n) => n.condition === 'attention') && (
          <li>
            <svg viewBox="0 0 14 14"><circle cx="7" cy="7" r="5.5" fill="var(--ink)" stroke="var(--down)" stroke-width="1.5" /><path d="M7 4.2v3.3M7 9.6v.01" stroke="var(--paper)" stroke-width="1.4" stroke-linecap="round" /></svg>Needs attention
          </li>
        )}
        {nodes.some((n) => n.isThis) && (
          <li>
            <svg viewBox="0 0 14 14"><circle cx="7" cy="7" r="5.5" fill="none" stroke="var(--ink-2)" stroke-dasharray="1 2" /></svg>This computer
          </li>
        )}
      </ul>
    </figure>
  );
}
