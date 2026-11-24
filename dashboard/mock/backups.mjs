// Mock backups (dashboard/API.md §13): every state the Backups page, the
// setup step and the overview card show. Shapes and words follow
// internal/agent/backup_api.go and backup_jobs.go.
import { randomBytes } from 'node:crypto';

const H = 3_600_000;
const D = 24 * H;
const GB = 1024 ** 3;
const iso = (t) => new Date(t).toISOString().replace(/\.\d{3}Z$/, 'Z');
const SPEED = Number(process.env.MOCK_SPEED || 1);

export const DATA_DIR = '/var/lib/coyote/node-a';
export const DEFAULT_FOLDER = DATA_DIR + '-backups';

/** Backup state kinds a scenario can start in. */
export function makeBackup(kind = 'healthy', o = {}) {
  const now = Date.now();
  const b = {
    enabled: kind !== 'off',
    dest: { kind: 'folder', label: DEFAULT_FOLDER, per_node: true, same_disk: true },
    encrypted: false,
    key: '',
    s3: null,
    schedule: { enabled: kind !== 'off', every: '24h0m0s', at: '03:00', keep: '168h0m0s', archive_timeout: '1m0s' },
    list: [],
    running: null,
    nextAt: null,
    problem: '',
    lastError: '',
    failingSince: null,
    check: null,
    restore: null,
    checkFails: !!o.checkFails,
    node: o.node ?? 'node-a',
    createdAt: now,
  };
  if (kind === 'healthy' || kind === 'attention' || kind === 's3' || kind === 'off' || kind === 'check-fails') {
    // A full backup every night at 03:00 for the last 7 days.
    const last3 = new Date();
    last3.setHours(3, 0, 41, 0);
    if (last3.getTime() > now) last3.setDate(last3.getDate() - 1);
    for (let d = 6; d >= 0; d--) {
      const fin = last3.getTime() - d * D;
      b.list.unshift({ id: idFor(fin, b.node), kind: 'scheduled', state: 'done', node: b.node, started_at: iso(fin - 4 * 60_000), finished_at: iso(fin), size_bytes: Math.round((1.18 - d * 0.012) * GB), data_bytes: Math.round((3.4 - d * 0.03) * GB) });
    }
    const next = new Date(last3.getTime() + D);
    b.nextAt = next.getTime();
  }
  if (kind === 'first') b.nextAt = now + 8 * 60_000;
  if (kind === 's3') {
    b.dest = { kind: 's3', label: 's3://bakery-backups/coyote (https://s3.us-west-004.backblazeb2.com)', per_node: false };
    b.encrypted = true;
    b.key = 'cybk-' + randomBytes(24).toString('base64url');
    b.s3 = { endpoint: 'https://s3.us-west-004.backblazeb2.com', bucket: 'bakery-backups', prefix: 'coyote', access_key: '004a1b2c3d4e5f', secret_key: 'kept' };
    for (const x of b.list) x.node = 'node-b';
  }
  if (kind === 'attention') {
    b.failingSince = now - 47 * 60_000;
    b.lastError = 'cannot write to /mnt/usb/coyote-backups: no such file or directory (the drive is not connected)';
    b.dest = { kind: 'folder', label: '/mnt/usb/coyote-backups', per_node: true, same_disk: false };
    b.problem = `Saving changes to the backup has failed since ${new Date(b.failingSince).toLocaleString()}: ${b.lastError}`;
  }
  if (kind === 'incomplete') {
    Object.assign(b, makeBackup('healthy', o));
    b.list[1].state = 'incomplete';
  }
  return b;
}

// The newest saved change: like the agent, it moves once a minute (the
// archive timeout), not on every tick.
const lastSaved = () => Math.floor((Date.now() - 20_000) / 60_000) * 60_000;

function idFor(t, node) {
  return new Date(t).toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, '') + '-' + node;
}

function restorable(b) {
  const done = b.list.filter((x) => x.state === 'done');
  if (!b.enabled || done.length === 0) return undefined;
  const oldest = done[done.length - 1];
  return { from: oldest.finished_at, to: iso(b.failingSince ?? lastSaved()) };
}

export function statusView(b) {
  const done = b.list.filter((x) => x.state === 'done');
  const v = { enabled: b.enabled, destination: { ...b.dest }, encrypted: b.encrypted, archive: { waiting_bytes: 0, lag_ms: 0 } };
  if (!b.enabled) return v;
  if (done[0]) v.last_backup = done[0];
  const r = restorable(b);
  if (r) v.restorable = r;
  if (b.running) v.running = b.running;
  if (b.nextAt) v.next_backup_at = iso(b.nextAt);
  v.archive = { last_archived: '00000001000000000000002A', last_archived_at: iso(b.failingSince ?? lastSaved()), waiting_bytes: b.failingSince ? 14 * 16 * 1024 * 1024 : 0, lag_ms: b.failingSince ? Math.floor((Date.now() - b.failingSince) / 60_000) * 60_000 : 0 };
  if (b.failingSince) v.archive.failing_since = iso(b.failingSince);
  if (b.lastError) v.last_error = b.lastError;
  if (b.problem) v.problem = b.problem;
  return v;
}

export function backupsView(b) {
  const out = { schedule: { ...b.schedule }, destination: { ...b.dest }, status: statusView(b), backups: b.running ? [b.running, ...b.list] : [...b.list] };
  if (b.check) out.check = { ...b.check, lines: [...b.check.lines] };
  if (b.restore) out.restore = { ...b.restore, lines: [...b.restore.lines] };
  return out;
}

export class BackupError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

const running = (b) => b.check?.state === 'running' || b.restore?.state === 'running';

/** Advance a running backup, check or restore. */
export function tickBackup(b, dt) {
  if (!b) return;
  const ms = dt * SPEED;
  if (b.running) {
    b.running.elapsed = (b.running.elapsed ?? 0) + ms;
    if (b.running.elapsed > 9000) {
      const fin = Date.now();
      b.list.unshift({ id: idFor(fin, b.running.node), kind: 'manual', state: 'done', node: b.running.node, started_at: b.running.started_at, finished_at: iso(fin), size_bytes: Math.round(1.19 * GB), data_bytes: Math.round(3.42 * GB) });
      b.running = null;
      if (!b.nextAt || b.nextAt < Date.now()) b.nextAt = Date.now() + D;
    }
  }
  for (const j of [b.check, b.restore]) {
    if (!j || j.state !== 'running') continue;
    j.elapsed = (j.elapsed ?? 0) + ms;
    const steps = j.kind === 'check' ? CHECK_LINES : RESTORE_LINES;
    const n = Math.min(steps.length, 1 + Math.floor(j.elapsed / 1500));
    while (j.lines.length < n) j.lines.push(steps[j.lines.length - 1] ?? '');
    if (j.elapsed > 1500 * steps.length) finishJob(b, j);
  }
}

const RESTORE_LINES = [
  'Using the full backup taken by node-a; unpacking 3.4 GB.',
  'Unpacked. Replaying the saved changes up to the moment asked for…',
  'Reached the moment. Making the copy writable.',
  'Stopped the restored copy. It is ready to start.',
];
const CHECK_LINES = [
  'Using the newest full backup; unpacking 3.4 GB.',
  'Replaying every saved change after it…',
  'Restored. Reading every page with amcheck...',
];

function finishJob(b, j) {
  delete j.elapsed;
  j.finished_at = iso(Date.now());
  j.took_ms = Date.now() - Date.parse(j.started_at);
  const done = b.list.find((x) => x.state === 'done');
  j.backup_id = done?.id;
  j.last_change = new Date(j.target ? Date.parse(j.target) - 2000 : Date.now() - 30_000).toISOString().replace('T', ' ').replace(/\.\d+Z$/, '+00');
  if (j.kind === 'check') {
    if (b.checkFails) {
      j.state = 'failed';
      j.damage = ['table public.orders: page 812 has a bad checksum'];
      j.error = 'the restored copy has damage: table public.orders: page 812 has a bad checksum';
    } else {
      j.state = 'done';
      j.databases = 3;
      j.relations = 811;
    }
    return;
  }
  j.state = 'done';
  j.next_command = `coyote up --data-dir ${j.into.includes(' ') ? `"${j.into}"` : j.into} --pg-port 7532 --client-port 5532 --listen 0.0.0.0:7581 --control-port 7580`;
}

// ---- routes ----

export function backupNow(b) {
  if (!b.enabled) throw new BackupError(409, 'backups_off', 'Backups are off. Turn them on with `coyote backup on`.');
  if (!b.running) b.running = { id: '', kind: 'running', state: 'running', node: b.node, started_at: iso(Date.now()), size_bytes: 0 };
  return { message: `Full backup started on ${b.node}.`, nodes: [b.node] };
}

const parseDur = (s) => {
  const m = /^(\d+)d$/.exec(s);
  if (m) return Number(m[1]) * D;
  const re = /(\d+(?:\.\d+)?)(h|m|s)/g;
  let t = 0;
  let any = '';
  for (const x of s.matchAll(re)) {
    t += Number(x[1]) * { h: H, m: 60_000, s: 1000 }[x[2]];
    any += x[0];
  }
  if (any !== s || !s) throw new BackupError(400, 'bad_request', `${JSON.stringify(s)}: expected a duration like 24h or 7d`);
  return t;
};
const goDur = (ms) => {
  const h = Math.floor(ms / H);
  const m = Math.floor((ms % H) / 60_000);
  const s = Math.floor((ms % 60_000) / 1000);
  return `${h}h${m}m${s}s`;
};

export function setSchedule(b, req) {
  if (req.enabled !== undefined) {
    b.enabled = !!req.enabled;
    b.schedule.enabled = b.enabled;
    if (b.enabled && !b.nextAt) b.nextAt = Date.now() + 10 * 60_000;
    if (!b.enabled) b.problem = '';
  }
  const every = req.every ? parseDur(req.every) : parseDur(b.schedule.every);
  const keep = req.keep ? parseDur(req.keep) : parseDur(b.schedule.keep);
  if (every < H || every > 31 * D) throw new BackupError(400, 'bad_request', 'every must be between 1 hour and 31 days');
  if (keep < every) throw new BackupError(400, 'bad_request', 'keep must be at least as long as every');
  if (req.at && !/^([01]\d|2[0-3]):[0-5]\d$/.test(req.at)) throw new BackupError(400, 'bad_request', 'at must be a time like 03:00');
  if (req.every) b.schedule.every = goDur(every);
  if (req.keep) b.schedule.keep = goDur(keep);
  if (req.at) b.schedule.at = req.at;
  if (req.archive_timeout) b.schedule.archive_timeout = goDur(parseDur(req.archive_timeout));
  return { ...b.schedule };
}

/** POST /v1/backups/check-destination: same rules as checkDest in backup_jobs.go. */
export function checkDestination(req) {
  const out = { ok: false, kind: req.kind, label: '', encrypted: req.kind === 's3' || !!req.encrypt };
  if (req.kind === 'folder') {
    const dir = (req.folder || DEFAULT_FOLDER).replace(/\/+$/, '');
    if (!dir.startsWith('/') && !/^[A-Za-z]:\\/.test(dir)) return { ...out, problem: 'Type the full path of the folder, for example /mnt/backup-disk/coyote-backups.' };
    out.folder = out.label = dir;
    if (dir === DATA_DIR || dir.startsWith(DATA_DIR + '/') || DATA_DIR.startsWith(dir + '/')) return { ...out, problem: `Pick a folder outside the database's own folder (${DATA_DIR}): backups inside it would be lost with it.` };
    if (dir.startsWith('/readonly') || dir.startsWith('/System')) return { ...out, problem: `This computer's coyote cannot write in ${dir}. Pick another folder, or give it permission to write there.` };
    if (dir.startsWith('/full')) return { ...out, problem: 'That disk has only 812 MB free. Backups need at least 2 GB; free some space or pick another disk.', free_bytes: 812 * 1024 * 1024 };
    const other = dir.startsWith('/mnt/') || dir.startsWith('/Volumes/') || dir.startsWith('/media/');
    out.ok = true;
    out.exists = dir === '/mnt/usb/coyote-backups' || dir === DEFAULT_FOLDER;
    out.free_bytes = other ? 1.7 * 1024 * GB : 412 * GB;
    out.total_bytes = other ? 1.8 * 1024 * GB : 1000 * GB;
    out.same_disk = !other;
    if (!out.exists) out.warnings = ['The folder does not exist yet; it is created when you save.'];
    return out;
  }
  if (req.kind === 's3') {
    const s = req.s3;
    if (!s) return { ...out, problem: 'Fill in the storage address, the bucket and the access keys.' };
    if (!s.endpoint?.startsWith('http')) return { ...out, problem: 'The storage address must start with https:// (for example https://s3.eu-central-1.amazonaws.com).' };
    if (!s.bucket) return { ...out, problem: 'Fill in the bucket name.' };
    if (!s.access_key || !s.secret_key) return { ...out, problem: 'Fill in the access key ID and the secret key.' };
    out.label = `s3://${s.bucket}/${s.prefix || ''} (${s.endpoint})`.replace('/ (', ' (');
    if (s.secret_key === 'wrong') return { ...out, problem: 'The storage refused these keys. Check the access key ID and the secret key, and that the key may read, write and delete in this bucket. (403 SignatureDoesNotMatch)' };
    if (s.bucket === 'missing') return { ...out, problem: 'That bucket was not found at this address. Check the bucket name and that the address is for the bucket’s region. (404 NoSuchBucket)' };
    if (/nowhere/.test(s.endpoint)) return { ...out, problem: 'Could not reach the storage address from this computer. Check the address and the internet connection. (dial tcp: lookup nowhere.example: no such host)' };
    out.ok = true;
    if (s.endpoint.startsWith('http://')) out.warnings = ['This address is not https. Backups are encrypted before they leave this computer, but the access keys travel unprotected. Use https if the storage offers it.'];
    return out;
  }
  return { ...out, problem: 'Choose "folder" or "s3".' };
}

export function setDestination(b, req) {
  const c = checkDestination(req.kind === 's3' && req.s3 && !req.s3.secret_key && b.s3 ? { ...req, s3: { ...req.s3, secret_key: b.s3.secret_key } } : req);
  if (!c.ok) throw new BackupError(400, 'destination_failed', `That backup destination does not work from ${b.node}: ${c.problem}`);
  const hadKey = !!b.key;
  if (req.kind === 's3') {
    b.dest = { kind: 's3', label: c.label, per_node: false };
    b.s3 = { ...req.s3, secret_key: req.s3.secret_key || b.s3?.secret_key };
    b.encrypted = true;
  } else {
    b.dest = { kind: 'folder', label: c.folder, per_node: true, same_disk: !!c.same_disk };
    b.encrypted = !!req.encrypt;
  }
  if (b.encrypted && !b.key) b.key = 'cybk-' + randomBytes(24).toString('base64url');
  // Saving works again at the new place.
  b.failingSince = null;
  b.lastError = '';
  b.problem = '';
  return { destination: { ...b.dest }, encrypted: b.encrypted, ...(!hadKey && b.encrypted ? { new_key: b.key } : {}) };
}

export function revealKey(b) {
  if (!b.encrypted || !b.key) throw new BackupError(404, 'no_key', 'Backups are not encrypted, so there is no backup key.');
  return { key: b.key };
}

export function startCheck(b) {
  if (!b.enabled) throw new BackupError(409, 'backups_off', 'Backups are off. Turn them on first.');
  if (running(b)) throw new BackupError(409, 'job_running', 'A backup check or restore is already running on this computer. Wait for it to finish.');
  b.check = { kind: 'check', state: 'running', started_at: iso(Date.now()), lines: [`Restoring the newest backup into a temporary folder on ${b.node}.`] };
  return { ...b.check };
}

/** POST /v1/backups/restore: dry run or start. */
export function restore(b, req) {
  if (!b.enabled) throw new BackupError(409, 'backups_off', 'Backups are off. Turn them on first.');
  const stampNow = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  const p = { ok: false, suggested_into: `/var/lib/coyote/coyote-restore-${stampNow.getFullYear()}${pad(stampNow.getMonth() + 1)}${pad(stampNow.getDate())}-${pad(stampNow.getHours())}${pad(stampNow.getMinutes())}` };
  const win = restorable(b);
  if (!win) return finishPlan(req, { ...p, problem: 'There is no finished full backup yet, so there is nothing to restore. Take one with Back up now, then try again.' });
  let target;
  if (req.to) {
    target = Date.parse(req.to);
    if (!Number.isFinite(target)) return finishPlan(req, { ...p, problem: 'That moment is not a date and time.' });
    if (target < Date.parse(win.from)) return finishPlan(req, { ...p, problem: `That is before the oldest moment the backups reach (${new Date(win.from).toLocaleString()}). Pick a later moment.` });
    if (target > Date.parse(win.to)) return finishPlan(req, { ...p, problem: `That is after the newest saved change (${new Date(win.to).toLocaleString()}). Pick an earlier moment, or restore everything saved.` });
    p.target = iso(target);
  }
  const chosen = b.list.find((x) => x.state === 'done' && (!target || Date.parse(x.finished_at) < target));
  p.backup_id = chosen.id;
  p.need_bytes = chosen.data_bytes;
  const into = (req.into || (req.dry_run ? p.suggested_into : '')).replace(/\/+$/, '');
  if (!into) return finishPlan(req, { ...p, problem: 'Choose a new folder to restore into.' });
  if (!into.startsWith('/')) return finishPlan(req, { ...p, problem: `Type the full path of a new folder, for example ${p.suggested_into}.` });
  p.into = into;
  if (into === DATA_DIR || into.startsWith(DATA_DIR + '/') || DATA_DIR.startsWith(into + '/')) return finishPlan(req, { ...p, problem: "That is this database's own folder (or holds it). A restore only goes into a new folder, so the running database is never touched." });
  if (b.dest.kind === 'folder' && (into === b.dest.label || into.startsWith(b.dest.label + '/'))) return finishPlan(req, { ...p, problem: 'That is inside the backup folder. Pick a new folder somewhere else.' });
  if (into === '/var/lib/coyote' || into.startsWith('/etc') || into === '/home/sam/notes') return finishPlan(req, { ...p, problem: `${into} is not empty. A restore only goes into a new or empty folder, so nothing is ever overwritten. Pick another name.` });
  p.free_bytes = 412 * GB;
  p.ok = true;
  if (req.dry_run) return p;
  if (running(b)) throw new BackupError(409, 'job_running', 'A backup check or restore is already running on this computer. Wait for it to finish.');
  b.restore = { kind: 'restore', state: 'running', started_at: iso(Date.now()), into, ...(p.target ? { target: p.target } : {}), lines: [`Restoring into ${into}. The running database is not touched.`] };
  return { ...b.restore };
}

function finishPlan(req, p) {
  if (req.dry_run) return p;
  throw new BackupError(400, 'restore_refused', p.problem);
}
