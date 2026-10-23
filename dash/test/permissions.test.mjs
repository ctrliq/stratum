import assert from 'node:assert/strict';
import test from 'node:test';
import { harness, nodes, text } from './harness.mjs';

test('login is a token field: no OIDC, no username and no password', async () => {
  const h = await harness();
  const Login = await h.component('screens/login.jsx', 'Login');
  const tree = Login({ config: { mode: 'standalone', auth: { required: true, providers: ['token'] } } });

  const inputs = nodes(tree, 'TextInput');
  assert.equal(inputs.length, 1, 'exactly one field: the bearer token');
  assert.equal(inputs[0].props.name, 'token');
  // A credential must not be echoed on screen, and the browser must not be
  // invited to remember it as a password for this origin.
  assert.equal(inputs[0].props.type, 'password');
  assert.equal(inputs[0].props.autoComplete, 'off');

  const buttons = nodes(tree, 'Button');
  assert.equal(buttons.length, 1, 'only the submit button');
  assert.equal(buttons[0].props.type, 'submit');

  const rendered = text(tree).toLowerCase();
  for (const gone of ['sso', 'password', 'username']) {
    assert.equal(rendered.includes(gone), false, `login still mentions "${gone}"`);
  }
  // Even with a provider list that names them, which an older server might.
  const stale = text(Login({ config: { auth: { required: true, providers: ['local', 'oidc'] } } })).toLowerCase();
  assert.equal(stale.includes('sso'), false, 'an oidc provider in /config must not bring the SSO button back');
});

test('login surfaces a rejected token without blaming the network', async () => {
  const h = await harness();
  const Login = await h.component('screens/login.jsx', 'Login');
  const tree = Login({ config: { auth: { required: true } } });
  const alert = nodes(tree, 'div').find((node) => node.props.role === 'alert');
  assert.equal(text(alert), '', 'no error before an attempt');
  // The router search string is not read at all: there is no IdP
  // redirect to carry an error back from.
  const withQuery = await harness({ search: '?error=oidc_failed&error_description=secret' });
  const L2 = await withQuery.component('screens/login.jsx', 'Login');
  assert.equal(text(L2({ config: { auth: { required: true } } })).includes('secret'), false);
});

test('writes are offered for any session, and denied without one', async () => {
  for (const [config, signedIn, allowed] of [
    // A config nobody served (here: the 502) is a visible production error,
    // not the login-free fallback — that fallback is dev-only, so this realm
    // (no __STRATUM_DEV__) must deny writes.
    [null, false, false],
    [{ mode: 'standalone', auth: { required: false } }, false, true],  // explicitly unauthenticated
    [{ mode: 'standalone', auth: { required: true } }, false, false],  // auth required, signed out
    [{ mode: 'standalone', auth: { required: true } }, true, true],    // auth required, signed in
  ]) {
    const h = await harness({ config, signedIn });
    assert.equal(h.session.canWrite(), false, 'unresolved config must deny');
    await h.resolveConfig();
    if (signedIn) await h.signIn();
    assert.equal(h.session.canWrite(), allowed);
    assert.equal(h.session.useCanWrite(), allowed);
  }
});

test('login and fetchMe expose the same identity without the CSRF token', async () => {
  const h = await harness({ signedIn: true });
  await h.session.login('a-bearer-token');
  const loginIdentity = JSON.stringify(h.session.currentIdentity());
  assert.equal(h.session.getCsrfToken(), 'csrf');
  await h.session.fetchMe();
  assert.equal(JSON.stringify(h.session.currentIdentity()), loginIdentity);
  assert.deepEqual(Object.keys(h.session.currentIdentity()).sort(), ['groups', 'provider', 'username']);
  assert.equal(h.session.getCsrfToken(), 'csrf');
  await h.session.logout();
  assert.equal(h.session.currentIdentity(), null);
  assert.equal(h.session.getCsrfToken(), null);
});

// The identity carries no role, so nothing in the SPA may reintroduce one.
test('the identity carries no roles field', async () => {
  const h = await harness({ signedIn: true });
  await h.session.fetchMe();
  assert.equal('roles' in h.session.currentIdentity(), false);
  assert.equal(typeof h.session.isAdmin, 'undefined', 'isAdmin() claimed a privilege level this build has no notion of');
  assert.equal(typeof h.session.useIsAdmin, 'undefined');
});

for (const signedIn of [false, true]) {
  test(`${signedIn ? 'signed in' : 'signed out'}: drawer and palette controls match permission`, async () => {
    const h = await harness({ config: { mode: 'standalone', auth: { required: true } }, signedIn });
    await h.resolveConfig();
    if (signedIn) await h.signIn();
    const Drawer = await h.component('ui/exec-logs-drawer.jsx', 'ExecLogsDrawer');
    const drawer = Drawer({ tabs: [
      { key: 'pod', kind: 'pod', pod: 'demo', ns: 'default', sub: 'exec' },
      { key: 'node', kind: 'node', node: 'worker' },
    ], activeKey: 'pod' });
    assert.equal(nodes(drawer, 'Button').some((b) => b.props.title === 'Exec'), signedIn);
    assert.equal(nodes(drawer, 'ExecTerminal').length, signedIn ? 1 : 0);
    assert.equal(nodes(drawer, 'NodeShell').length, signedIn ? 1 : 0);
    assert.ok(nodes(drawer, 'Button').some((b) => b.props.title === 'Logs'));
    const Palette = await h.component('ui/palette.jsx', 'CommandPalette');
    assert.equal(text(Palette({ open: true })).includes('Open shell in pod'), signedIn);
  });
}

// The node shell is gated twice. This covers the UI half: the control is
// hidden unless the server said it enabled it. The server half (refusing the
// pod create and the exec before they reach the apiserver) is the real gate,
// and lives in internal/dashboard.
for (const nodeShell of [false, true]) {
  test(`host shell control is offered only when the server enables it (nodeShell=${nodeShell})`, async () => {
    const h = await harness({ config: { mode: 'standalone', auth: { required: true }, features: { nodeShell } }, signedIn: true });
    await h.resolveConfig();
    await h.signIn();
    assert.equal((await h.module('cluster-context.js')).nodeShellEnabled(), nodeShell);

    const Nodes = await h.component('screens/infra.jsx', 'Nodes');
    const NodeDetail = await h.component('screens/infra.jsx', 'NodeDetail');
    const node = { name: 'worker-0', status: 'Ready', ready: true, version: 'v1.34.0', age: '1d', roles: [], cordoned: false };

    const titles = nodes(Nodes({ data: { NODES: [node] }, onAction: () => {} }), 'Button').map((b) => b.props.title);
    assert.equal(titles.includes('Open host shell'), nodeShell);
    // Cordon stays available either way: it is an ordinary write, not the
    // privileged host shell.
    assert.ok(titles.includes('Cordon'));

    // The detail panel offers the same shell from its own footer, so it needs
    // the same gate: covering only the table row would leave a second,
    // ungated way in.
    const labels = nodes(NodeDetail({ node, data: {}, onAction: () => {} }), 'Button').map((b) => text(b));
    assert.equal(labels.includes('Host shell'), nodeShell);

    // A node that is not Ready cannot host the shell whatever the flag says:
    // the two guards answer different questions and both have to hold.
    const notReady = { ...node, ready: false, status: 'NotReady' };
    const offline = nodes(Nodes({ data: { NODES: [notReady] }, onAction: () => {} }), 'Button').map((b) => b.props.title);
    assert.equal(offline.includes('Open host shell'), false);
    const offlineFooter = nodes(NodeDetail({ node: notReady, data: {}, onAction: () => {} }), 'Button').map((b) => text(b));
    assert.equal(offlineFooter.includes('Host shell'), false);
  });
}

// Writes through /k8s go to the same server, behind the same CSRF middleware
// as /api/v1 — so they need the same header, or every cordon, delete and
// node-shell create 403s the moment the proxy sits behind a session.
test('k8s writes carry the CSRF header and reads do not', async () => {
  const h = await harness({ config: { mode: 'standalone', auth: { required: true } }, signedIn: true });
  await h.resolveConfig();
  await h.signIn();
  assert.equal(h.session.getCsrfToken(), 'csrf');

  const k8s = await h.module('k8sutil.js');
  for (const [label, call] of [
    ['POST', () => k8s.k8sPost('/api/v1/namespaces/default/pods', { kind: 'Pod' })],
    // PUT has its own helper, and the method list that decides which verbs are
    // unsafe must cover it too: Edit-YAML saves a ConfigMap through k8sPut.
    ['PUT', () => k8s.k8sPut('/api/v1/namespaces/default/configmaps/demo', { data: {} })],
    ['DELETE', () => k8s.k8sDelete('/api/v1/namespaces/default/pods/demo')],
    ['PATCH', () => k8s.k8sPatch('/api/v1/nodes/worker-0', { spec: {} })],
  ]) {
    h.calls.length = 0;
    await call();
    assert.equal(h.calls.length, 1, `${label} made no request`);
    const headers = h.calls[0].init.headers || {};
    assert.equal(headers['X-Stratum-CSRF'], 'csrf', `${label} through /k8s carried no CSRF token`);
  }

  h.calls.length = 0;
  await k8s.k8sGet('/api/v1/pods', null);
  assert.equal(h.calls.length, 1);
  assert.equal((h.calls[0].init.headers || {})['X-Stratum-CSRF'], undefined, 'a read must not need a CSRF token');

  // And with no session there is no token to send, rather than a stale one.
  await h.session.logout();
  h.calls.length = 0;
  await k8s.k8sDelete('/api/v1/namespaces/default/pods/demo');
  assert.equal((h.calls[0].init.headers || {})['X-Stratum-CSRF'], undefined);
});
