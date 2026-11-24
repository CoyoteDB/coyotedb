// A minimal observable store with a Preact hook. Enough for one app-wide
// state object; no dependency needed.
import { useEffect, useState } from 'preact/hooks';

export interface Store<S> {
  get(): S;
  set(patch: Partial<S> | ((s: S) => Partial<S>)): void;
  subscribe(fn: (s: S) => void): () => void;
}

export function createStore<S extends object>(initial: S): Store<S> {
  let state = initial;
  const subs = new Set<(s: S) => void>();
  return {
    get: () => state,
    set(patch) {
      const p = typeof patch === 'function' ? patch(state) : patch;
      state = { ...state, ...p };
      for (const fn of subs) fn(state);
    },
    subscribe(fn) {
      subs.add(fn);
      return () => subs.delete(fn);
    },
  };
}

export function useStore<S extends object, R>(store: Store<S>, pick: (s: S) => R): R {
  const [v, setV] = useState(() => pick(store.get()));
  useEffect(() => {
    setV(pick(store.get()));
    return store.subscribe((s) => setV(() => pick(s)));
    // pick is expected to be stable in meaning; re-subscribing on each render is not needed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [store]);
  return v;
}
