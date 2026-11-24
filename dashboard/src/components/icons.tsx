// Line icons, 24×24, stroke = currentColor. Decorative unless given a title.
import type { JSX, SVGAttributes } from 'preact';
import type { Tone } from '../lib/health';

type P = SVGAttributes<SVGSVGElement> & { title?: string };

function Svg({ title, children, ...rest }: P & { children: JSX.Element | JSX.Element[] }) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"
      aria-hidden={title ? undefined : 'true'} role={title ? 'img' : undefined} focusable="false" {...rest}>
      {title ? <title>{title}</title> : null}
      {children}
    </svg>
  );
}

export const Check = (p: P) => <Svg {...p}><path d="M5 12.5l4.2 4.2L19 7" /></Svg>;
export const CheckCircle = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="9" /><path d="M8 12.3l2.7 2.7L16.2 9.5" /></Svg>;
export const Cross = (p: P) => <Svg {...p}><path d="M6 6l12 12M18 6L6 18" /></Svg>;
export const CrossCircle = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="9" stroke-dasharray="3 2.4" /><path d="M9 9l6 6M15 9l-6 6" /></Svg>;
export const Warn = (p: P) => <Svg {...p}><path d="M12 3.5L21.5 20h-19L12 3.5z" /><path d="M12 10v4.5M12 17.2v.1" /></Svg>;
export const Info = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="9" /><path d="M12 11v5.5M12 7.8v.1" /></Svg>;
export const Half = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="9" /><path d="M12 3a9 9 0 0 1 0 18z" fill="currentColor" stroke="none" /></Svg>;
export const Swap = (p: P) => <Svg {...p}><path d="M4 8h13l-3.5-3.5M20 16H7l3.5 3.5" /></Svg>;
export const Copy = (p: P) => <Svg {...p}><rect x="8.5" y="8.5" width="11" height="11" rx="1.5" /><path d="M15.5 8.5V5.5a1 1 0 0 0-1-1h-9a1 1 0 0 0-1 1v9a1 1 0 0 0 1 1h3" /></Svg>;
export const Menu = (p: P) => <Svg {...p}><path d="M4 7h16M4 12h16M4 17h16" /></Svg>;
export const Sun = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="4" /><path d="M12 2.5v2M12 19.5v2M4.6 4.6l1.4 1.4M18 18l1.4 1.4M2.5 12h2M19.5 12h2M4.6 19.4L6 18M18 6l1.4-1.4" /></Svg>;
export const Moon = (p: P) => <Svg {...p}><path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z" /></Svg>;
export const Spinner = (p: P) => (
  <Svg {...p}><circle cx="12" cy="12" r="8.5" opacity="0.25" /><path class="spin" d="M12 3.5a8.5 8.5 0 0 1 8.5 8.5" /></Svg>
);
export const Pending = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="8.5" stroke-dasharray="2 3" /></Svg>;
export const Arrow = (p: P) => <Svg {...p}><path d="M5 12h14M13 6l6 6-6 6" /></Svg>;
export const Plus = (p: P) => <Svg {...p}><path d="M12 5v14M5 12h14" /></Svg>;
export const Eye = (p: P) => <Svg {...p}><path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z" /><circle cx="12" cy="12" r="2.8" /></Svg>;
export const Lock = (p: P) => <Svg {...p}><rect x="5" y="10.5" width="14" height="10" rx="1.5" /><path d="M8 10.5V7.5a4 4 0 0 1 8 0v3" /></Svg>;
export const Plug = (p: P) => <Svg {...p}><path d="M9 3v5M15 3v5M6.5 8h11v3a5.5 5.5 0 0 1-11 0zM12 16.5V21" /></Svg>;
export const Clock = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></Svg>;
export const Shield = (p: P) => <Svg {...p}><path d="M12 3l7.5 3v5.5c0 4.6-3.2 8.2-7.5 9.5-4.3-1.3-7.5-4.9-7.5-9.5V6z" /></Svg>;
export const Dot = (p: P) => <Svg {...p}><circle cx="12" cy="12" r="3" fill="currentColor" stroke="none" /></Svg>;


export function ToneIcon({ tone, ...p }: P & { tone: Tone }) {
  switch (tone) {
    case 'ok':
      return <CheckCircle {...p} />;
    case 'one-copy':
      return <Half {...p} />;
    case 'switching':
      return <Swap {...p} />;
    case 'attention':
      return <Warn {...p} />;
    case 'down':
      return <CrossCircle {...p} />;
  }
}
