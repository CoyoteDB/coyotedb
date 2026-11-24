// Development only (import.meta.env.DEV): a scenario switcher for the mock
// server. Vite removes this from production builds.
import { useEffect, useState } from 'preact/hooks';

export function DevBar() {
  const [list, setList] = useState<{ id: string; label: string }[]>([]);
  const [cur, setCur] = useState('');
  useEffect(() => {
    void fetch('/__mock/scenarios')
      .then((r) => r.json() as Promise<{ current: string; scenarios: { id: string; label: string }[] }>)
      .then((d) => {
        setList(d.scenarios);
        setCur(d.current);
      })
      .catch(() => undefined);
  }, []);
  if (!list.length) return null;
  return (
    <div class="devbar">
      <label>
        mock{' '}
        <select
          value={cur}
          onChange={(e) => {
            const id = e.currentTarget.value;
            setCur(id);
            void fetch('/__mock/scenario', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) }).then(() =>
              location.reload(),
            );
          }}
        >
          {list.map((s) => (
            <option key={s.id} value={s.id}>
              {s.label}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}
