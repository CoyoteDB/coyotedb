// Light theme by default, dark on request; remembered per browser.
// public/theme-init.js applies the saved choice before first paint.
import { useState } from 'preact/hooks';
import * as I from '../components/icons';

const KEY = 'coyote-theme';

export function currentTheme(): 'light' | 'dark' {
  return document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light';
}

export function setTheme(t: 'light' | 'dark'): void {
  document.documentElement.dataset.theme = t;
  try {
    localStorage.setItem(KEY, t);
  } catch {
    /* private mode: the choice lasts for this page only */
  }
}

export function ThemeToggle() {
  const [t, setT] = useState(currentTheme);
  const next = t === 'dark' ? 'light' : 'dark';
  return (
    <button
      type="button"
      class="icon-btn"
      aria-label={`Switch to ${next} theme`}
      title={`Switch to ${next} theme`}
      onClick={() => {
        setTheme(next);
        setT(next);
      }}
    >
      {t === 'dark' ? <I.Sun /> : <I.Moon />}
    </button>
  );
}
