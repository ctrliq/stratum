import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, collect, text } from './harness.mjs';

test('the header shows the part of an identity after its last colon', async () => {
  const h = await harness();
  const shortIdentity = await h.component('ui/shell.jsx', 'shortIdentity');
  assert.equal(shortIdentity('system:serviceaccount:stratum-dashboard:stratum-dashboard-admin'), 'stratum-dashboard-admin');
  assert.equal(shortIdentity('oidc:alice@example.com'), 'alice@example.com');
  assert.equal(shortIdentity('alice'), 'alice', 'a name with no colon is shown whole');
  assert.equal(shortIdentity('odd:'), 'odd:', 'a trailing colon would leave nothing, so the whole name stays');
  assert.equal(shortIdentity('kubectl proxy'), 'kubectl proxy');
  assert.equal(shortIdentity(undefined), '');
});

test('the user button shows the short name and keeps the full identity in its tooltip and menu', async () => {
  const h = await harness();
  const AppShell = await h.component('ui/shell.jsx', 'AppShell');
  const full = 'system:serviceaccount:stratum-dashboard:stratum-dashboard-admin';
  const tree = AppShell({ active: 'overview', onNavigate: () => {}, crumbs: ['Overview'], namespace: 'all-namespaces',
    config: { links: {}, auth: { required: true } }, me: { username: full, provider: 'kubernetes' },
    cluster: { name: 'kube' }, onSignOut: () => {}, children: null });
  const menu = collect(tree, (n) => typeof n.type === 'function' && n.type.name === 'UserMenu')[0];
  const rendered = menu.type(menu.props);
  const button = rendered.props.trigger;
  assert.equal(button.props.title, full);
  assert.match(text(button), /stratum-dashboard-admin/);
  assert.doesNotMatch(text(button), /serviceaccount/);
  assert.ok(text(rendered.props.children).includes(full), 'the menu still names the full identity');
});
