// Small typed client for /v1 (see API.md). Same-origin only; the session
// cookie travels by itself, the CSRF token is kept in memory.
import type * as T from './types';

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: Record<string, unknown> | undefined;
  constructor(status: number, body: T.ApiErrorBody) {
    super(body.error);
    this.name = 'ApiError';
    this.status = status;
    this.code = body.code;
    this.details = body.details;
  }
}

/** The agent could not be reached at all (stopped, network, wrong port). */
export class UnreachableError extends Error {
  constructor(cause?: unknown) {
    super('The CoyoteDB program on this computer is not answering.', { cause });
    this.name = 'UnreachableError';
  }
}

type Listener = (kind: 'unauthenticated' | 'unreachable' | 'reachable') => void;

let csrf = '';
const listeners = new Set<Listener>();

export function onConnectionEvent(l: Listener): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

function emit(kind: Parameters<Listener>[0]): void {
  for (const l of listeners) l(kind);
}

export function setCsrf(token: string): void {
  csrf = token;
}

const UNSAFE = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

export async function request<R>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<R> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (UNSAFE.has(method) && csrf) headers['X-Coyote-CSRF'] = csrf;
  let res: Response;
  try {
    res = await fetch('/v1' + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    });
  } catch (err) {
    if (signal?.aborted) throw err;
    emit('unreachable');
    throw new UnreachableError(err);
  }
  emit('reachable');
  if (res.status === 204) return undefined as R;
  const text = await res.text();
  let data: unknown = undefined;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = undefined;
    }
  }
  if (!res.ok) {
    const b = data as Partial<T.ApiErrorBody> | undefined;
    const errBody: T.ApiErrorBody = {
      error: typeof b?.error === 'string' && b.error ? b.error : `The program answered ${res.status} ${res.statusText}.`,
      code: typeof b?.code === 'string' && b.code ? b.code : 'http_' + res.status,
      details: b?.details,
    };
    // 401 on anything but the sign-in calls means the session ended.
    if (res.status === 401 && !path.startsWith('/session')) emit('unauthenticated');
    throw new ApiError(res.status, errBody);
  }
  return data as R;
}

const get = <R>(p: string, s?: AbortSignal) => request<R>('GET', p, undefined, s);
const post = <R>(p: string, b: unknown = {}) => request<R>('POST', p, b);

export const api = {
  // session
  signIn: (token: string) => request<T.Session>('POST', '/session', { token }),
  session: () => get<T.Session>('/session'),
  signOut: () => request<undefined>('DELETE', '/session'),
  version: () => get<T.VersionInfo>('/version'),

  // setup
  setup: () => get<T.Setup>('/setup'),
  addresses: () => get<T.Addresses>('/addresses'),
  checkInvite: (code: string, probe = true) => post<T.InviteCheck>('/setup/check-invite', { code, probe }),
  create: (r: T.CreateRequest) => post<T.Setup>('/setup/create', r),
  join: (r: T.JoinRequest) => post<T.Setup>('/setup/join', r),
  resetSetup: () => post<T.Setup>('/setup/reset'),

  // cluster
  status: (s?: AbortSignal) => get<T.Status>('/status', s),
  events: (before?: string, limit = 50) =>
    get<T.EventsPage>(`/events?limit=${limit}${before ? '&before=' + encodeURIComponent(before) : ''}`),
  createInvite: (r: T.InviteRequest) => post<T.InviteResponse>('/invites', r),
  invites: () => get<{ invites: T.OpenInvite[] }>('/invites'),
  /** 204 (also when it was already cancelled); 404 unknown_invite; 409 secret_used / secret_expired. */
  cancelInvite: (id: string) => request<undefined>('DELETE', '/invites/' + encodeURIComponent(id)),
  removeMember: (node: string) => request<{ message: string }>('DELETE', '/members/' + encodeURIComponent(node)),
  renameMember: (node: string, name: string) => request<T.MemberView>('PATCH', '/members/' + encodeURIComponent(node), { name }),
  leave: () => post<{ message: string }>('/leave'),
  switchTo: (to: string) => post<T.SwitchResult>('/switch', { to }),
  promote: (confirm: string) => post<T.SwitchResult>('/promote', { confirm, accept_data_loss: true }),
  settings: () => get<T.Settings>('/settings'),
  setSettings: (set: Record<string, string>) => post<T.Settings>('/settings', { set }),
  node: () => get<T.NodeInfo>('/node'),
  setNode: (r: { dashboard_remote: boolean }) => request<T.NodeInfo>('PATCH', '/node', r),
  connection: () => get<T.Connection>('/connection'),
  revealPassword: () => post<{ password: string }>('/connection/reveal'),
  newAppPassword: () => post<T.ConnInfo>('/app-password'),
  problemCodes: () => get<{ codes: T.ProblemCode[] }>('/problem-codes'),
  setAddresses: (node: string, addresses: string[], force = false) =>
    post<T.SetAddressResult>('/members/' + encodeURIComponent(node) + '/addresses', { addresses, force }),

  // M7
  backups: (refresh = false) => get<T.Backups>(refresh ? '/backups?refresh=1' : '/backups'),
  runBackup: () => post<{ message: string; nodes: string[] }>('/backups'),
  setBackupSchedule: (s: Partial<Pick<T.BackupSchedule, 'enabled' | 'every' | 'at' | 'keep' | 'archive_timeout'>>) => request<T.BackupSchedule>('PUT', '/backups/schedule', s),
  setBackupDestination: (r: T.BackupDestinationRequest) => request<T.BackupDestinationResult>('PUT', '/backups/destination', r),
  /** Tries a destination without saving it; also served before setup. */
  checkBackupDestination: (r: T.BackupDestinationRequest) => post<T.BackupDestCheck>('/backups/check-destination', r),
  revealBackupKey: () => post<{ key: string }>('/backups/key/reveal'),
  checkBackup: () => post<T.BackupJob>('/backups/check'),
  planRestore: (r: { to?: string; into?: string }) => post<T.RestorePlan>('/backups/restore', { ...r, dry_run: true }),
  restore: (r: { to?: string; into: string }) => post<T.BackupJob>('/backups/restore', r),
  agents: () => get<T.Agents>('/agents'),
  createAgentKey: (name: string, access: T.AgentKey['access']) => post<{ key: T.AgentKey; secret: string }>('/agents/keys', { name, access }),
  revokeAgentKey: (id: string) => request<{ message: string }>('DELETE', '/agents/keys/' + encodeURIComponent(id)),
};

export function errorCode(err: unknown): string {
  if (err instanceof ApiError) return err.code;
  if (err instanceof UnreachableError) return 'unreachable_agent';
  return 'unknown';
}

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return 'Something went wrong.';
}
