// Types for the CoyoteDB local HTTP API, version 1.
// Kept in lockstep with dashboard/API.md: every change there changes this file.
// Fields marked "new" in API.md are optional here, so the dashboard also
// works against an agent that serves only today's fields.

export type Time = string; // RFC 3339, UTC

export interface ApiErrorBody {
  error: string;
  code: string;
  details?: Record<string, unknown>;
}

export interface Platform {
  os: string; // GOOS
  arch: string; // GOARCH
}

export type Role = 'data' | 'witness';

// ---- session (§2) ----

export interface Session {
  csrf_token: string;
  expires_at: Time;
  absolute_expires_at: Time;
  idle_timeout_s: number;
}

// ---- version (§15) ----

export interface VersionInfo {
  product: string;
  version: string;
  commit: string;
  built_at?: Time;
  go_version: string;
  os: string;
  arch: string;
  postgres_version: string;
  api: number;
  /** Which parts of API.md this agent serves for real (false: hidden, the route answers 501). */
  features?: Partial<Record<FeatureKey, boolean>>;
  /** The kind of container the agent runs in ("docker", ...); absent outside one. PostgreSQL is then part of the image (D68). */
  container?: string;
}

export type FeatureKey =
  | 'setup' | 'invite_probe' | 'invite_role' | 'invite_cancel' | 'rename' | 'forced_promote'
  | 'cluster_name' | 'remote_dashboard' | 'app_password' | 'set_address' | 'backups' | 'agents';

// ---- setup (§4) ----

/** The real agent sends fresh, creating, joining, member or failed; 'left' and 'removed' are kept for older mocks and future agents. */
export type SetupState = 'fresh' | 'creating' | 'joining' | 'member' | 'failed' | 'left' | 'removed';
export type StepId = 'contact' | 'download' | 'verify' | 'prepare' | 'start' | 'copy' | 'follow';
export type StepState = 'pending' | 'active' | 'done' | 'failed' | 'skipped';

export interface Step {
  id: StepId;
  state: StepState;
  percent?: number;
  bytes_done?: number;
  bytes_total?: number;
  detail?: string;
}

export interface Progress {
  operation: 'create' | 'join';
  role: Role;
  /** Not sent by today's agent. */
  cluster_name?: string;
  /** Not sent by today's agent. */
  from?: string;
  started_at: Time;
  done: boolean;
  steps: Step[];
}

export interface Setup {
  state: SetupState;
  role: Role | null;
  suggested_name: string;
  platform: Platform;
  progress?: Progress | null;
  error?: ApiErrorBody | null;
  removed_reason?: string;
}

export type AddressClass = 'tailscale' | 'vpn' | 'lan' | 'link-local' | 'public' | 'shared' | 'other' | 'loopback';

export interface Address {
  addr: string;
  interface: string;
  class: AddressClass;
  label: string;
  suggested: boolean;
  wireless?: boolean;
  may_change?: boolean;
  warnings?: string[];
}

/** Set when the agent runs inside a container (D68): its own network's addresses are left out. */
export interface ContainerInfo {
  kind: string;
  /** No address found here is one other computers can use: ask for the host's. */
  host_address_needed: boolean;
  /** How many container-network addresses were left out. */
  hidden: number;
}

export interface Addresses {
  addresses: Address[];
  skipped?: { interface: string; addr?: string; reason: string }[];
  warnings?: string[];
  default?: string | null;
  container?: ContainerInfo | null;
}

/** check-invite's contact with the cluster. Nothing is sent, so the invite is not used up. */
export interface InviteProbe {
  /** An address answered as a member of the invite's cluster. */
  reachable: boolean;
  /** The address that answered. */
  via?: string;
  /** The answer was signed by the cluster the code was made for. Older agents do not send it. */
  cluster_verified?: boolean;
  /** A machine answered, but it is not the invite's cluster. */
  wrong_cluster?: boolean;
  /** Every address, when none answered as the cluster. */
  tried?: string[];
  /** One plain sentence about the result. */
  message?: string;
  // Not sent by today's agent (it cannot learn them yet); used if a later one does.
  cluster_name?: string;
  platform?: Platform | null;
  data_nodes?: number;
  witnesses?: number;
  compatible?: boolean;
}

export interface InviteCheck {
  valid: boolean;
  cluster_id: string;
  expires: Time;
  addresses: string[];
  probe: InviteProbe | null;
}

export interface CreateRequest {
  name: string;
  advertise: string;
  cluster_name?: string;
}

export interface JoinRequest {
  code: string;
  /** "" lets the agent use this computer's host name. */
  name: string;
  /** An IP address or host name (no port needed), or "" for automatic. */
  advertise: string;
  role: Role;
  /** `join --allow-mixed-os`: let the cluster check a different kind of computer (data computers only). */
  allow_mixed_os?: boolean;
}

// ---- status (§5) ----

export interface PGReport {
  running: boolean;
  port: number;
  role?: string;
  timeline?: number;
  streaming: boolean;
  stream_cut?: boolean;
  receive_lsn?: string;
  replay_lsn?: string;
  flush_lsn?: string;
  error?: string;
}

export type NodePhase = 'primary' | 'standby' | 'witness' | 'cloning' | 'rejoining' | 'waiting' | 'starting' | 'needs-attention' | '';

export interface NodeReport {
  id: string;
  name: string;
  role: Role;
  state: NodePhase;
  message?: string;
  epoch: number;
  last_primary_epoch?: number;
  rejoin_marker?: string;
  copy_progress?: number;
  postgres?: PGReport;
  cut_epoch?: number;
  cut_hold?: boolean;
  lease?: boolean;
}

export interface ClusterView {
  id: string;
  epoch: number;
  standby_eligible: boolean;
  standby_eligible_since?: Time;
  single_data_node_mode: 'strict' | 'degrade_after';
  primary_id: string;
  primary_name: string;
  leader_id: string;
  leader_name: string;
  sync_set: string[] | null;
  sync_k: number;
  // new
  name?: string;
  platform?: Platform | null;
  stale?: boolean;
}

export interface MemberView {
  id: string;
  name: string;
  role: Role;
  suffrage: 'voter' | 'nonvoter';
  status: 'active' | 'needs_reclone' | string;
  addresses: string[];
  primary: boolean;
  leader: boolean;
  this: boolean;
  in_sync_set: boolean;
  reachable: boolean;
  report?: NodeReport;
  replication?: string;
  lag_bytes: number;
  // new
  platform?: Platform;
  joined_at?: Time;
  last_seen?: Time | null;
  lag_seconds?: number | null;
  eligible?: boolean;
  eligible_reason?: string;
  agent_version?: string;
}

export interface ControllerView {
  lease_healthy: boolean;
  lease_age_ns: number;
  failover_blocked?: string;
  writes_waiting: boolean;
  writes_waiting_ns?: number;
  one_copy: boolean;
  notes?: string[];
}

export interface ControllerReport {
  leader: boolean;
  view: ControllerView;
  last_action?: string;
  last_action_at?: Time;
  last_error?: string;
}

export type FailoverCode = '' | 'two_nodes' | 'no_quorum' | 'no_eligible_copy' | 'waiting_lease' | 'strict_waiting';

export interface Switching {
  kind: 'planned' | 'automatic' | 'forced';
  from: string;
  to: string;
  started_at: Time;
  step: 'waiting_lease' | 'cutting_streams' | 'catching_up' | 'promoting' | 'reconnecting';
  takeover_in_ms?: number;
}

export interface PromoteInfo {
  allowed: boolean;
  since?: Time;
  may_lose_since?: Time;
  reason?: string;
}

export interface Health {
  main_node: string;
  lease: string;
  copies: string;
  writes_wait: boolean;
  one_copy: boolean;
  one_copy_since?: Time;
  failover?: string;
  // new
  failover_code?: FailoverCode;
  writes_blocked?: boolean;
  writes_blocked_since?: Time;
  degrade_in_ms?: number;
  switching?: Switching | null;
  promote?: PromoteInfo;
}

export interface Status {
  self: string;
  /** This node's backups (data nodes; §13). */
  backup?: BackupStatus;
  cluster: ClusterView;
  nodes: MemberView[];
  warnings: string[];
  /** Problems with a stable code (§5.1). Absent on older agents: treat as []. */
  problems?: Problem[];
  controller?: ControllerReport;
  health: Health;
}

// ---- problems with a stable code (§5.1, §17) ----

export interface ProblemAddress {
  /** HOST:PORT the cluster records for the node (the previous address). */
  recorded: string[];
  /** What that computer has now, when known. */
  now?: string[];
  /** What to save instead; the "Use …" button uses these. */
  suggested?: string[];
  /** Names of the nodes that cannot reach it. */
  unreachable_from?: string[];
  /** It reaches the others, they cannot reach it. */
  one_way?: boolean;
}

export interface Problem {
  code: string;
  severity: 'problem' | 'warning';
  /** The computer it is about; absent or "" for the whole cluster. */
  node?: string;
  /** One plain short line (server words). */
  summary: string;
  /** One plain sentence: what to do. */
  fix?: string;
  /** Long explanation; paragraphs separated by a blank line. */
  details: string;
  evidence?: string[];
  /** Exact CLI fix commands, in order. */
  commands?: string[];
  since?: string;
  address?: ProblemAddress;
  /** Self-contained Markdown for an AI agent, built by the server (no secrets). */
  markdown: string;
}

export interface ProblemCode {
  code: string;
  title: string;
  meaning: string;
  causes: string[];
  fix: string;
  commands: string[];
}

// ---- activity (§7) ----

export type EventKind =
  | 'cluster_created' | 'member_joined' | 'member_left' | 'member_removed' | 'member_renamed'
  | 'node_unreachable' | 'node_back' | 'switch_started' | 'switch_failed' | 'main_changed'
  | 'forced_promote' | 'one_copy_started' | 'one_copy_ended' | 'writes_blocked' | 'writes_resumed'
  | 'copy_started' | 'copy_finished' | 'needs_reclone' | 'invite_created' | 'invite_cancelled'
  | 'settings_changed' | 'agent_started';

export interface ActivityEvent {
  id: string;
  at: Time;
  kind: EventKind | string;
  severity: 'info' | 'good' | 'warning' | 'problem';
  node?: string;
  from?: string;
  reason?: string;
  role?: Role;
  message: string;
}

export interface EventsPage {
  events: ActivityEvent[];
  more: boolean;
}

// ---- invites (§8) ----

export interface InviteRequest {
  ttl?: string;
  role?: Role;
}

export interface InviteResponse {
  code: string;
  expires: Time;
  id?: string;
  role?: Role;
  command?: string;
  platform?: Platform | null;
  /** This node runs in a container: what to run on the next computer, with the same image (D68). */
  docker?: DockerJoin | null;
}

export interface DockerJoin {
  /** Starts the next computer's container with its setup page; the code is pasted there. */
  setup_command: string;
  /** Prints that page's sign-in link. */
  link_command: string;
  /** Joins without the page: the code in COYOTE_INVITE; NAME and ADDRESS to fill in. */
  cli_command: string;
}

export interface OpenInvite {
  id: string;
  role: Role;
  created_at: Time;
  expires: Time;
  /** True for a used and for a cancelled invite. */
  used: boolean;
  used_by?: string;
  /** Cancelled from a dashboard (used is true too). */
  cancelled?: boolean;
}

// ---- switch / promote (§10) ----

export interface SwitchResult {
  from: string;
  to: string;
  epoch: number;
  message: string;
  forced?: boolean;
}

// ---- settings / node (§11) ----

export interface Settings {
  lease_duration: string;
  lease_renew_interval: string;
  single_data_node_mode: 'strict' | 'degrade_after';
  degrade_after: string;
  gone_after: string;
  cluster_name?: string;
}

export interface NodeInfo {
  id: string;
  name: string;
  role: Role;
  platform: Platform;
  data_dir: string;
  advertise: string[];
  ports: { postgres: number; api: number; peer: number };
  service: boolean;
  dashboard: { remote: boolean; urls: string[] };
}

// ---- connection (§12) ----

export interface ConnHost {
  node: string;
  host: string;
  port: number;
  this: boolean;
  reachable: boolean;
}

export interface Connection {
  database: string;
  user: string;
  sslmode: string;
  hosts: ConnHost[];
  /** Null on a witness (no database here). */
  local: { host: string; port: number } | null;
  /** Path of the cluster CA certificate on this computer (for sslrootcert). */
  ca_file?: string;
  /** The CA certificate itself (PEM, public), for apps on other computers. */
  ca_pem?: string;
  /** The application role is not set up yet; `note` says why. */
  pending?: boolean;
  note?: string;
}

/** POST /v1/app-password: the application role's new credentials. */
export interface ConnInfo {
  url: string;
  hosts: string[];
  user: string;
  database: string;
  password: string;
  ca_file: string;
  note?: string;
  pending?: boolean;
}

// ---- addresses of a member (POST /v1/members/{node}/addresses) ----

export interface AddressProbe {
  addr: string;
  ok: boolean;
  rtt_ms?: number;
  error?: string;
}

export interface SetAddressResult {
  node: string;
  probes: AddressProbe[];
  /** False: nothing changed (no address answered and force was not set). */
  recorded: boolean;
  message: string;
}

// ---- backups / agents (§13, §14; M7) ----

/** One full backup (§13). `done`: restorable; `incomplete`: never used for a restore. */
export interface Backup {
  id: string;
  kind: 'scheduled' | 'manual' | 'running' | string;
  state: 'running' | 'done' | 'incomplete';
  node: string;
  started_at: Time;
  finished_at?: Time;
  size_bytes: number;
  data_bytes?: number;
}

export interface BackupDestination {
  kind: 'folder' | 's3' | string;
  label: string;
  /** Every data node keeps its own complete set (folder). */
  per_node: boolean;
  /** A folder on the disk that holds the data (protects from mistakes, not from that disk failing). */
  same_disk?: boolean;
}

export interface BackupStatus {
  enabled: boolean;
  destination: BackupDestination;
  encrypted: boolean;
  last_backup?: Backup;
  restorable?: { from: Time; to: Time; gap_after?: string };
  running?: Backup | null;
  next_backup_at?: Time;
  archive: {
    last_archived?: string;
    last_archived_at?: Time;
    waiting_bytes: number;
    lag_ms: number;
    failing_since?: Time | null;
    conflicts?: number;
  };
  gap?: { from: string; to: string; at: Time; reason: string; closed: boolean } | null;
  last_error?: string;
  last_error_at?: Time | null;
  /** One plain sentence when something needs the person. */
  problem?: string;
}

/** Go duration strings ("24h0m0s"). */
export interface BackupSchedule {
  enabled: boolean;
  every: string;
  at: string;
  keep: string;
  archive_timeout: string;
}

export interface BackupJob {
  kind: 'check' | 'restore';
  state: 'running' | 'done' | 'failed';
  started_at: Time;
  finished_at?: Time;
  lines: string[];
  into?: string;
  target?: Time;
  backup_id?: string;
  last_change?: string;
  stop_line?: string;
  databases?: number;
  relations?: number;
  damage?: string[];
  took_ms?: number;
  error?: string;
  next_command?: string;
}

export interface Backups {
  schedule: BackupSchedule;
  destination: BackupDestination;
  status: BackupStatus;
  backups: Backup[];
  check?: BackupJob;
  restore?: BackupJob;
}

export interface BackupS3 {
  endpoint: string;
  bucket: string;
  prefix?: string;
  region?: string;
  access_key: string;
  /** Write-only. Empty keeps the saved one (same access key). */
  secret_key: string;
  path_style?: string;
}

export interface BackupDestinationRequest {
  kind: 'folder' | 's3';
  /** Full path; "" = next to each node's data folder. */
  folder?: string;
  s3?: BackupS3;
  encrypt?: boolean;
}

export interface BackupDestCheck {
  ok: boolean;
  problem?: string;
  kind: string;
  label: string;
  folder?: string;
  exists?: boolean;
  free_bytes?: number;
  total_bytes?: number;
  same_disk?: boolean;
  encrypted: boolean;
  warnings?: string[];
}

export interface BackupDestinationResult {
  destination: BackupDestination;
  encrypted: boolean;
  /** Only when this call made the backup key: show it once. */
  new_key?: string;
}

export interface RestorePlan {
  ok: boolean;
  problem?: string;
  into?: string;
  suggested_into: string;
  exists?: boolean;
  free_bytes?: number;
  need_bytes?: number;
  backup_id?: string;
  target?: Time;
  warnings?: string[];
}

export interface AgentKey {
  id: string;
  name: string;
  access: 'read_only' | 'read_write';
  created_at: Time;
  last_used_at?: Time;
}

export interface Agents {
  mcp: { url: string; transport: string };
  keys: AgentKey[];
}
