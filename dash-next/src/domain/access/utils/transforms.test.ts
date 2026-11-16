import { describe, expect, it } from 'vitest';

import { toBindingRow, toRulesRow, toServiceAccountRow } from './transforms';

describe('toServiceAccountRow', () => {
  it('counts the secrets the account references, 0 when none', () => {
    expect([
      toServiceAccountRow({ secrets: [{ name: 'token-a' }, { name: 'token-b' }] }),
      toServiceAccountRow({}),
    ]).toEqual([{ secrets: 2 }, { secrets: 0 }]);
  });
});

describe('toRulesRow', () => {
  it('counts the rules present on the role, 0 when none', () => {
    expect([
      toRulesRow({ rules: [{ verbs: ['get'] }, { verbs: ['list'] }, { verbs: ['watch'] }] }),
      toRulesRow({}),
    ]).toEqual([{ rules: 3 }, { rules: 0 }]);
  });
});

describe('toBindingRow', () => {
  it('names the role referred to without saying which kind of role it is', () => {
    expect(
      toBindingRow({
        roleRef: { apiGroup: 'rbac.authorization.k8s.io', kind: 'ClusterRole', name: 'view' },
      }),
    ).toEqual({ role: 'view' });
  });

  it('shows the absent mark when the binding refers to no role', () => {
    expect(toBindingRow({})).toEqual({ role: '—' });
  });
});
