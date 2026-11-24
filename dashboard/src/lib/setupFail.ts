// Plain words and next steps for a create or join that stopped (API.md §4).
// The agent's own `error` text is written for the command line (it may name
// flags like --name); the setup page says what to do on the page instead.
import type { ApiErrorBody, Platform, Role } from '../api/types';
import { platformLabel } from './format';
import { decodeInvite, inviteProblemText } from './invite';

export type Path = 'create' | 'join';
/** What a failed setup offers: "Try again", "Start over", and for some failures one or two more. */
export type FailAction = 'retry' | 'mixed' | 'witness' | 'reset';

/** The cluster addresses in a pasted code, if it decodes. */
export function inviteAddrs(code: string): string[] {
  if (!code.trim()) return [];
  const r = decodeInvite(code);
  return r.ok ? r.invite.addresses : [];
}

const NEW_INVITE = 'Make a new invite on a computer in the cluster (Add a computer in its dashboard, or run “coyote invite”), then try again with the new code.';

/**
 * Plain words for every error a create or join can end with, written for
 * this page (the agent's own `error` is written for the command line). Each
 * ends with what to do next.
 */
export function explainError(e: ApiErrorBody, addrs: string[] = []): string {
  const d = e.details as { cluster?: Platform; this?: Platform; tried?: string[] } | undefined;
  switch (e.code) {
    case 'duplicate_name':
      return 'Another computer in the cluster already has this name. Pick another name for this computer, then try again.';
    case 'secret_used':
      return `This invite code was already used by another computer. Each code works once. ${NEW_INVITE}`;
    case 'secret_expired':
    case 'invite_expired':
      return `This invite code has expired. ${NEW_INVITE}`;
    case 'secret_unknown':
      return `The cluster does not recognise this invite code. It may have been cancelled. ${NEW_INVITE}`;
    case 'not_invite':
      return inviteProblemText('not_invite');
    case 'invite_corrupt':
      return inviteProblemText('corrupt');
    case 'invite_version':
      return inviteProblemText('version');
    case 'wrong_cluster':
      return 'A computer answered, but it is not the cluster this code was made for, so nothing was sent to it. Check that the code is the newest one, or make a new invite on a computer in the cluster.';
    case 'unreachable': {
      const tried = d?.tried?.length ? d.tried : addrs;
      const port = tried.map((a) => /:(\d+)$/.exec(a)?.[1]).find(Boolean) ?? '7481';
      return `None of the cluster’s computers answered${tried.length ? ` (tried ${tried.join(', ')})` : ''}. Check that the computer that made the code is running, that both computers are on the same network (or Tailscale), and that its firewall lets port ${port} in. Then try again.`;
    }
    case 'mixed_os_off':
      return 'This computer is a different kind (system or processor) than the cluster’s data computers, and mixing kinds is off. You can ask the cluster to check this computer and let it in only if it stores data exactly like the others, or join as a witness, which holds no data.';
    case 'mixed_os_blocked':
      return 'The cluster cannot take a different kind of computer right now: one of its data computers reports a problem. Fix that first (its dashboard says what), then try again.';
    case 'mixed_os_not_ready':
      return 'The cluster could not check this computer yet. Wait a minute, then try again.';
    case 'platform_mismatch':
      return d?.cluster && d.this
        ? `The cluster’s data computers run ${platformLabel(d.cluster)}, and this one runs ${platformLabel(d.this)}. PostgreSQL’s files only work between identical systems. This computer can join as a witness instead, which holds no data.`
        : 'This computer cannot hold a copy of the data: its database programs store or compare data differently from the cluster’s data computers. It can still join as a witness, which holds no data.';
    case 'joiner_too_old':
      return 'This computer runs an older version of CoyoteDB than the cluster. Update CoyoteDB on this computer, then try again.';
    case 'wrong_role':
      return 'This code was made for the other kind of computer (data computer or witness). Make a new code of the right kind, then try again.';
    case 'port_in_use':
      return 'Another program on this computer uses a port CoyoteDB needs. Stop that program, or stop “coyote setup” and start it again with other ports (“coyote help” lists them). Then try again.';
    case 'disk_full':
      return 'This computer’s disk is full. Free some space, then try again.';
    case 'unfinished_join':
      return 'This computer started joining a cluster but did not finish. Join again with the same invite code to finish, or start over.';
    case 'copy_failed':
      return `Copying the data stopped: ${e.error} Nothing on the other computers changed. Try again.`;
    case 'already_set_up':
      return 'This computer is already being set up. Reload the page to see how it is going.';
    case 'unreachable_agent':
      return 'CoyoteDB on this computer is not answering. Check that “coyote setup” is still running, then try again.';
  }
  const msg = e.error.trim();
  return /[.!?]$/.test(msg) ? `${msg} When that is fixed, try again.` : `${msg}. When that is fixed, try again.`;
}

/** The extra choices a failure offers (besides Try again and Start over). */
export function failActions(code: string, op: 'create' | 'join', role: Role): FailAction[] {
  if (op !== 'join' || role !== 'data') return [];
  if (code === 'mixed_os_off') return ['mixed', 'witness'];
  if (code === 'platform_mismatch') return ['witness'];
  return [];
}

/** Where "Try again" goes for a failure: the step whose input most likely needs changing. Inputs are kept. */
export function retryStage(code: string, path: Path, haveCode: boolean): 'invite' | 'name' | 'address' {
  if (path === 'join' && !haveCode) return 'invite';
  switch (code) {
    case 'duplicate_name':
      return 'name';
    case 'secret_used':
    case 'secret_expired':
    case 'secret_unknown':
    case 'not_invite':
    case 'invite_corrupt':
    case 'invite_expired':
    case 'invite_version':
    case 'wrong_cluster':
    case 'unfinished_join':
      return path === 'join' ? 'invite' : 'address';
  }
  return 'address';
}

