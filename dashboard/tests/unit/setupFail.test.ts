import { describe, expect, it } from 'vitest';
import { explainError, failActions, retryStage } from '../../src/lib/setupFail';

// Every code the agent can end a create or join with (internal/agent/setup.go
// setupError, internal/cluster/errors.go, check-invite / join 4xx answers).
const CODES = [
  'secret_unknown', 'secret_used', 'secret_expired', 'duplicate_name', 'platform_mismatch', 'mixed_os_off', 'mixed_os_blocked',
  'mixed_os_not_ready', 'joiner_too_old', 'wrong_cluster', 'unreachable', 'port_in_use', 'disk_full', 'unfinished_join', 'failed',
  'not_invite', 'invite_corrupt', 'invite_expired', 'invite_version', 'bad_request', 'already_set_up', 'not_yet_available', 'unreachable_agent',
];
const CLI_WORDS = /--[a-z]|`/;

describe('setup failures', () => {
  it('every code gets one plain explanation that ends with a next step', () => {
    for (const code of CODES) {
      const t = explainError({ code, error: 'could not do the thing: run the same command again with --name x' });
      expect(t, code).toMatch(/[.!?]$/);
      expect(t.length, code).toBeGreaterThan(20);
      // Known codes speak for the page, not the command line.
      if (!['failed', 'bad_request', 'not_yet_available'].includes(code)) expect(t, code).not.toMatch(CLI_WORDS);
    }
  });

  it('says what to do for the common ones', () => {
    expect(explainError({ code: 'duplicate_name', error: '' })).toMatch(/Pick another name for this computer/);
    for (const code of ['secret_used', 'secret_expired', 'secret_unknown']) expect(explainError({ code, error: '' })).toMatch(/Make a new invite on a computer in the cluster \(Add a computer/);
    expect(explainError({ code: 'unreachable', error: '' }, ['100.64.0.41:7555'])).toMatch(/tried 100\.64\.0\.41:7555.*firewall lets port 7555 in/);
    expect(explainError({ code: 'port_in_use', error: '' })).toMatch(/Another program on this computer uses a port/);
    expect(explainError({ code: 'disk_full', error: '' })).toMatch(/Free some space/);
    expect(explainError({ code: 'mixed_os_off', error: '' })).toMatch(/join as a witness/);
  });

  it('falls back to the agent words with a next step', () => {
    expect(explainError({ code: 'failed', error: 'The database could not start' })).toBe('The database could not start. When that is fixed, try again.');
    expect(explainError({ code: 'failed', error: 'It broke.' })).toBe('It broke. When that is fixed, try again.');
  });

  it('offers "join anyway" and "witness" only where they help', () => {
    expect(failActions('mixed_os_off', 'join', 'data')).toEqual(['mixed', 'witness']);
    expect(failActions('platform_mismatch', 'join', 'data')).toEqual(['witness']);
    expect(failActions('mixed_os_off', 'join', 'witness')).toEqual([]);
    expect(failActions('secret_used', 'join', 'data')).toEqual([]);
    expect(failActions('mixed_os_off', 'create', 'data')).toEqual([]);
  });

  it('"Try again" goes back to the input that needs changing', () => {
    expect(retryStage('duplicate_name', 'join', true)).toBe('name');
    expect(retryStage('secret_used', 'join', true)).toBe('invite');
    expect(retryStage('unfinished_join', 'join', false)).toBe('invite');
    expect(retryStage('port_in_use', 'join', true)).toBe('address');
    expect(retryStage('port_in_use', 'join', false)).toBe('invite'); // the code is gone after a reload
    expect(retryStage('disk_full', 'create', false)).toBe('address');
    expect(retryStage('duplicate_name', 'create', false)).toBe('name');
  });
});
