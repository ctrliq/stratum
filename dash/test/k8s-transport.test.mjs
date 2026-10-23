import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';
import { transformWithOxc } from 'vite';

// k8sutil.js, live.js and session.js all talk to the server through the global
// fetch, and live.js stamps window on success, so all three have to exist
// before they are imported. The stub answers /api/v1/config from a mutable
// object and records every other URL it is asked for.
globalThis.window = {};
// This fixture exercises the document-local fallback. Cross-document session
// coordination runs against the native LockManager in signout.test.mjs.
Object.defineProperty(globalThis, 'navigator', { value: {}, configurable: true });
const stub = {
  config: null,                       // body served for /api/v1/config; null → 502
  calls: [],                          // every non-config request, in order
  answer: { ok: true, status: 200, body: { items: [] } },  // what a k8s call gets
  reject: null,                       // next request throws this instead
  hang: false,                        // requests never answer, and end only by abort
};
globalThis.fetch = async (url, opts) => {
  const path = String(url);
  if (path === '/api/v1/config') {
    return { ok: !!stub.config, status: stub.config ? 200 : 502, json: async () => stub.config };
  }
  stub.calls.push({ url: path, init: opts || {} });
  if (stub.reject) { const e = stub.reject; stub.reject = null; throw e; }
  // A request whose server accepts it and then stalls (half-open proxy) can
  // only end through the transport's signal — that is what the deadline is for.
  if (stub.hang) {
    return new Promise((resolve, reject) => {
      const signal = (opts || {}).signal;
      if (!signal) return;                    // no signal: nothing can end this request
      const abort = () => { const e = new Error('the request was aborted'); e.name = 'AbortError'; reject(e); };
      if (signal.aborted) return abort();
      signal.addEventListener('abort', abort);
    });
  }
  if (stub.answer.throw) throw stub.answer.throw;
  const body = stub.answer.body === undefined ? {} : stub.answer.body;
  return {
    ok: stub.answer.ok, status: stub.answer.status,
    json: async () => { if (stub.answer.bodyReadError) throw stub.answer.bodyReadError; return body; },
    text: async () => { if (stub.answer.bodyReadError) throw stub.answer.bodyReadError; return stub.answer.text !== undefined ? stub.answer.text : JSON.stringify(body); },
  };
};
console.warn = () => {}; console.log = () => {};

const { getConfig } = await import('../src/cluster-context.js');
const k8s = await import('../src/k8sutil.js');
const session = await import('../src/session.js');
const { loadLiveK8S } = await import('../src/live.js');

// Real timer reference captured before any test enables mock timers: the
// guard races below must measure real time, not mocked time.
const realSetTimeout = setTimeout;

// The proxy prefix is read per request, never captured at module load — the
// README states it and a downstream per-cluster client depends on it. So one
// call per verb plus a full load must all land under whatever prefix the
// config names *now*, and follow it when the config changes.
test('every Kubernetes call reads the proxy prefix when it is made', async () => {
  const runAll = async () => {
    await k8s.k8sGet('/api/v1/nodes', null);
    await k8s.k8sPost('/api/v1/namespaces/default/pods', { kind: 'Pod' });
    await k8s.k8sPut('/api/v1/namespaces/default/configmaps/demo', { data: {} });
    await k8s.k8sPatch('/api/v1/nodes/worker-0', { spec: {} });
    await k8s.k8sDelete('/api/v1/namespaces/default/pods/demo');
    await loadLiveK8S();
  };

  stub.config = { mode: 'standalone', k8sBase: '/clusters/a/k8s', auth: { required: false } };
  await getConfig({ force: true });
  stub.calls.length = 0;
  await runAll();
  const first = stub.calls.map((c) => c.url);
  assert.ok(first.length >= 6, 'every verb and the loader must have made a request');
  for (const url of first) {
    assert.equal(url.startsWith('/clusters/a/k8s/'), true, `${url} left the configured prefix`);
  }

  // The config now names another base: the same calls must follow it, with
  // none still going to the one the module first saw.
  stub.config = { mode: 'standalone', k8sBase: '/clusters/b/k8s', auth: { required: false } };
  await getConfig({ force: true });
  stub.calls.length = 0;
  await runAll();
  const second = stub.calls.map((c) => c.url);
  assert.ok(second.length >= 6);
  for (const url of second) {
    assert.equal(url.startsWith('/clusters/b/k8s/'), true, `${url} ignored the new prefix`);
    assert.equal(url.startsWith('/clusters/a/k8s/'), false, `${url} stayed on the old prefix`);
  }
});

// Every screen reads the result shapes of send and k8sGet: write toasts read
// r.error, the node-shell retry reads ok and status, and every kind list
// treats a k8sGet failure as "no data" through the default it passed in.
test('send and k8sGet keep their result shapes', async () => {
  const podBody = { kind: 'Pod', metadata: { name: 'demo' } };

  for (const [label, answer, expected] of [
    ['a 201 with a JSON body', { ok: true, status: 201, body: podBody },
      { ok: true, status: 201, body: podBody }],
    ['a 403 whose body carries a message', { ok: false, status: 403, body: { message: 'forbidden by RBAC' } },
      { ok: false, status: 403, error: 'forbidden by RBAC' }],
    ['a 500 with a plain-text body', { ok: false, status: 500, text: 'boom' },
      { ok: false, status: 500, error: 'boom' }],
    ['a 401', { ok: false, status: 401, body: { error: 'unauthenticated', message: 'sign in' } },
      { ok: false, status: 401, error: 'sign in' }],
    ['a rejected fetch', { throw: new Error('socket hung up') },
      { ok: false, status: 0, error: 'socket hung up' }],
  ]) {
    stub.answer = answer;
    const r = await k8s.k8sPost('/api/v1/namespaces/default/pods', podBody);
    assert.deepEqual(r, expected, `k8sPost under ${label} must resolve to the pinned shape`);
  }

  // k8sGet returns the parsed body on success and the very default on any
  // failure — a result object here would be read as data by the kind lists.
  const dflt = { items: ['sentinel'] };
  for (const [label, answer] of [
    ['a 200', { ok: true, status: 200, body: podBody }],
    ['a 401', { ok: false, status: 401 }],
    ['a 403', { ok: false, status: 403 }],
    ['a 500', { ok: false, status: 500 }],
    ['a rejected fetch', { throw: new Error('socket hung up') }],
  ]) {
    stub.answer = answer;
    const r = await k8s.k8sGet('/api/v1/pods', dflt);
    if (answer.ok) assert.deepEqual(r, podBody, `k8sGet under ${label} must return the parsed body`);
    else assert.equal(r, dflt, `k8sGet under ${label} must return the very default passed in`);
  }
});

// The transport itself — k8sFetch — owns the prefix, the CSRF header, the
// deadline and the 401 rule. The two guards above read those behaviors through
// the rebuilt helpers; the tests below read them through the primitive the
// helpers are built on, so a helper that bypassed the transport could not hide.
test('k8sFetch reads the proxy prefix when the call is made', async () => {
  const read = async () => {
    await k8s.k8sFetch('GET', '/api/v1/nodes');
    await k8s.k8sFetch('POST', '/api/v1/namespaces/default/pods', { body: { kind: 'Pod' } });
    await k8s.k8sFetch('PUT', '/api/v1/namespaces/default/configmaps/demo', { body: { data: {} } });
    await k8s.k8sFetch('PATCH', '/api/v1/nodes/worker-0', { body: { spec: {} } });
    await k8s.k8sFetch('DELETE', '/api/v1/namespaces/default/pods/demo');
  };

  stub.config = { mode: 'standalone', k8sBase: '/clusters/a/k8s', auth: { required: false } };
  await getConfig({ force: true });
  stub.answer = { ok: true, status: 200, body: {} };
  stub.calls.length = 0;
  await read();
  assert.ok(stub.calls.length >= 5, 'every verb must have made one request');
  for (const { url } of stub.calls) {
    assert.equal(url.startsWith('/clusters/a/k8s/'), true, `${url} left the configured prefix`);
  }

  // The config now names another base: the same primitive must follow it, with
  // nothing still going to the prefix it first saw.
  stub.config = { mode: 'standalone', k8sBase: '/clusters/b/k8s', auth: { required: false } };
  await getConfig({ force: true });
  stub.calls.length = 0;
  await read();
  for (const { url } of stub.calls) {
    assert.equal(url.startsWith('/clusters/b/k8s/'), true, `${url} ignored the new prefix`);
    assert.equal(url.startsWith('/clusters/a/k8s/'), false, `${url} stayed on the old prefix`);
  }
});

test('metadata reads can select their Accept header and server API base through the shared transport', async () => {
  stub.config = { mode: 'standalone', k8sBase: '/custom/k8s', auth: { required: true } };
  await getConfig({ force: true });
  stub.answer = { ok: true, status: 200, body: { items: [] } };
  stub.calls.length = 0;
  const accept = 'application/json;as=Table;g=meta.k8s.io;v=v1';
  await k8s.k8sFetch('GET', '/api/v1/secrets', { accept });
  await k8s.k8sFetch('GET', '/api/v1/secret-summaries', { base: '' });
  assert.equal(stub.calls[0].url, '/custom/k8s/api/v1/secrets');
  assert.equal(stub.calls[0].init.headers.Accept, accept);
  assert.equal(stub.calls[1].url, '/api/v1/secret-summaries');
  assert.equal(stub.calls[1].init.headers.Accept, 'application/json');
  assert.ok(stub.calls.every(({ init }) => init.signal && init.credentials === 'same-origin'));
});

test('k8sFetch carries the CSRF header on unsafe methods and never on GET', async () => {
  stub.config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true } };
  await getConfig({ force: true });
  stub.answer = { ok: true, status: 200, body: {} };
  session.setCsrfToken('csrf-transport');
  try {
    for (const [method, path, opts, contentType] of [
      ['POST', '/api/v1/namespaces/default/pods', { body: { kind: 'Pod' } }, 'application/json'],
      ['PUT', '/api/v1/namespaces/default/configmaps/demo', { body: { data: {} } }, 'application/json'],
      ['PATCH', '/api/v1/nodes/worker-0', { body: { spec: {} }, patchType: 'strategic' }, 'application/strategic-merge-patch+json'],
      ['DELETE', '/api/v1/namespaces/default/pods/demo', {}, null],
    ]) {
      stub.calls.length = 0;
      await k8s.k8sFetch(method, path, opts);
      assert.equal(stub.calls.length, 1, `${method} made no request`);
      const { init } = stub.calls[0];
      assert.equal((init.headers || {})['X-Stratum-CSRF'], 'csrf-transport', `${method} carried no CSRF token`);
      assert.equal(init.body, opts.body === undefined ? undefined : JSON.stringify(opts.body), `${method} must send a JSON body`);
      if (contentType === null) assert.equal((init.headers || {})['Content-Type'], undefined, `${method} with no body must set no content type`);
      else assert.equal((init.headers || {})['Content-Type'], contentType, `${method} must send its content type`);
    }

    stub.calls.length = 0;
    const r = await k8s.k8sFetch('GET', '/api/v1/nodes');
    assert.equal((stub.calls[0].init.headers || {})['X-Stratum-CSRF'], undefined, 'a GET must not carry the CSRF token');
    assert.deepEqual(r, { ok: true, status: 200, body: {} }, 'the transport resolves one result shape for reads and writes');

    // The pagehide cleanup fires its DELETE as the tab goes away, so the
    // keepalive option has to survive to the fetch it drives.
    stub.calls.length = 0;
    await k8s.k8sFetch('DELETE', '/api/v1/namespaces/default/pods/demo', { keepalive: true });
    assert.equal(stub.calls[0].init.keepalive, true, 'the keepalive option must reach fetch');
  } finally { session.setCsrfToken(null); }
});

test('a 401 from any Kubernetes call sends the user to sign-in, and a 403 does not', async () => {
  stub.config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true } };
  await getConfig({ force: true });
  const seen = [];
  session.setUnauthorizedHandler((path) => seen.push(path));
  try {
    stub.answer = { ok: false, status: 401, body: { error: 'unauthenticated', message: 'sign in' } };
    for (const [label, path, call, expected] of [
      ['k8sGet', '/api/v1/nodes', () => k8s.k8sGet('/api/v1/nodes', { sentinel: true }), { sentinel: true }],
      ['k8sPost', '/api/v1/namespaces/default/pods', () => k8s.k8sPost('/api/v1/namespaces/default/pods', { kind: 'Pod' }), { ok: false, status: 401, error: 'sign in' }],
      ['k8sPut', '/api/v1/namespaces/default/configmaps/demo', () => k8s.k8sPut('/api/v1/namespaces/default/configmaps/demo', { data: {} }), { ok: false, status: 401, error: 'sign in' }],
      ['k8sPatch', '/api/v1/nodes/worker-0', () => k8s.k8sPatch('/api/v1/nodes/worker-0', { spec: {} }), { ok: false, status: 401, error: 'sign in' }],
      ['k8sDelete', '/api/v1/namespaces/default/pods/demo', () => k8s.k8sDelete('/api/v1/namespaces/default/pods/demo'), { ok: false, status: 401, error: 'sign in' }],
      ['k8sFetch', '/api/v1/nodes', () => k8s.k8sFetch('GET', '/api/v1/nodes'), { ok: false, status: 401, error: 'sign in' }],
    ]) {
      seen.length = 0;
      const r = await call();
      assert.deepEqual(r, expected, `${label} must keep its pinned 401 result shape`);
      assert.deepEqual(seen, [path], `${label} must route a 401 to the unauthorized handler exactly once`);
    }

    // A 403 is RBAC refusing this user, and a 500 is a server fault: neither
    // means the session ended, so neither may hop to sign-in.
    stub.answer = { ok: false, status: 403, body: { message: 'forbidden by RBAC' } };
    seen.length = 0;
    await k8s.k8sPost('/api/v1/namespaces/default/pods', { kind: 'Pod' });
    await k8s.k8sGet('/api/v1/nodes', null);
    assert.deepEqual(seen, [], 'a 403 must not send the user to sign-in');
    stub.answer = { ok: false, status: 500, text: 'boom' };
    await k8s.k8sDelete('/api/v1/namespaces/default/pods/demo');
    assert.deepEqual(seen, [], 'a 500 must not send the user to sign-in');
  } finally { session.setUnauthorizedHandler(null); }
});

test('a successful GET with no JSON answer reports failure, while writes keep their result contract', async () => {
  try {
    for (const text of ['<!doctype html><title>proxy error</title>', '', 'null']) {
      stub.answer = { ok: true, status: 200, text };
      const r = await k8s.k8sFetch('GET', '/api/v1/pods');
      assert.equal(r.ok, false, 'an unreadable Kubernetes answer must not count as fresh data');
      assert.equal(r.status, 200, 'retain the received HTTP status for diagnostics');
      assert.match(r.error, /JSON/);
    }
    stub.answer = { ok: true, status: 204, text: '' };
    assert.deepEqual(await k8s.k8sDelete('/api/v1/namespaces/default/pods/demo'),
      { ok: true, status: 204, body: null });
  } finally { stub.answer = { ok: true, status: 200, body: { items: [] } }; }
});

test('a known 401 routes even when its response body cannot be read', async () => {
  const seen = [];
  session.setUnauthorizedHandler((path) => seen.push(path));
  try {
    stub.answer = { ok: false, status: 401, bodyReadError: new Error('body failed') };
    const result = await k8s.k8sFetch('GET', '/api/v1/nodes');
    assert.equal(result.status, 401, 'loader outcome accounting must retain the confirmed unauthorized status');
    assert.deepEqual(seen, ['/api/v1/nodes']);
  } finally {
    session.setUnauthorizedHandler(null);
    stub.answer = { ok: true, status: 200, body: { items: [] } };
  }
});

test('a never-answering Kubernetes call terminates at its deadline, and the caller signal composes with it', { timeout: 2000 }, async (t) => {
  stub.config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true } };
  await getConfig({ force: true });
  stub.hang = true;
  // The deadline must ride the mockable timer (global setTimeout), so the
  // clock can be advanced instead of waited. A minute covers any default the
  // transport may pin; the loader keeps its own 15s by passing timeoutMs.
  // Without a deadline the awaits below never settle and the runner's own 2s
  // timeout fails the test.
  t.mock.timers.enable({ apis: ['setTimeout'] });
  try {
    for (const method of ['GET', 'POST', 'PUT', 'PATCH', 'DELETE']) {
      stub.calls.length = 0;
      const opts = method === 'GET' ? {} : { body: { kind: 'Pod' } };
      const p = k8s.k8sFetch(method, '/api/v1/namespaces/default/pods', opts);
      t.mock.timers.tick(60_000);
      const r = await p;
      assert.deepEqual({ ok: r.ok, status: r.status }, { ok: false, status: 0 }, `${method} must resolve to the failed shape at its deadline`);
      assert.equal(typeof r.error, 'string', `${method} must report the abort as an error`);
      assert.equal(stub.calls[0].init.signal && stub.calls[0].init.signal.aborted, true, `${method} must reach fetch through an aborted signal`);
    }

    // A caller-supplied deadline bounds the request too, at its own duration.
    stub.calls.length = 0;
    const quick = k8s.k8sFetch('GET', '/api/v1/nodes', { timeoutMs: 25 });
    t.mock.timers.tick(30);
    const rq = await quick;
    assert.deepEqual({ ok: rq.ok, status: rq.status }, { ok: false, status: 0 }, 'a caller timeoutMs must bound the request');
    assert.equal(stub.calls[0].init.signal.aborted, true);

    // k8sGet keeps its contract under the same hang: the deadline fires, the
    // read returns the very default passed in.
    const dflt = { sentinel: true };
    const g = k8s.k8sGet('/api/v1/nodes', dflt);
    t.mock.timers.tick(60_000);
    assert.equal(await g, dflt, 'k8sGet must return the default when the deadline fires');
  } finally { stub.hang = false; }
});

test('a caller signal aborts the request even while a deadline is armed', async (t) => {
  stub.config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true } };
  await getConfig({ force: true });
  stub.hang = true;
  // Real-time guard: if the caller abort never reaches the request, fail in
  // two seconds instead of hanging.
  const stuck = new Promise((r) => realSetTimeout(r, 2000, 'stuck'));
  const settle = (p) => Promise.race([p, stuck]);
  try {
    // A caller abort mid-flight must reach the request without waiting for the
    // clock — through one composed signal, not the caller's own, or the
    // transport's deadline could not have been armed beside it.
    const ctl = new AbortController();
    const p = k8s.k8sFetch('GET', '/api/v1/nodes', { signal: ctl.signal });
    ctl.abort();
    const r = await settle(p);
    assert.notEqual(r, 'stuck', 'a caller abort must reach the request without waiting for the deadline');
    assert.deepEqual({ ok: r.ok, status: r.status }, { ok: false, status: 0 });
    assert.equal(stub.calls[0].init.signal.aborted, true, 'the request must have been aborted');
    assert.notEqual(stub.calls[0].init.signal, ctl.signal, 'the deadline and the caller signal must flow through one composed signal');

    // An already-aborted signal aborts at once, before the server is waited on.
    const gone = new AbortController();
    gone.abort();
    const p2 = k8s.k8sFetch('GET', '/api/v1/nodes', { signal: gone.signal });
    const r2 = await settle(p2);
    assert.notEqual(r2, 'stuck', 'an already-aborted caller signal must abort the request at once');
    assert.deepEqual({ ok: r2.ok, status: r2.status }, { ok: false, status: 0 });
  } finally { stub.hang = false; }
});

test('a delayed Kubernetes 401 from a previous session cannot invalidate a new login', async (t) => {
  let answerOld;
  t.mock.method(globalThis, 'fetch', async (url) => {
    if (String(url) === '/api/v1/me') return Response.json({ username: 'old', csrfToken: 'old-csrf' });
    if (String(url) === '/api/v1/logout') return new Response(null, { status: 204 });
    if (String(url) === '/api/v1/login') return Response.json({ user: { username: 'new' }, csrfToken: 'new-csrf' });
    return new Promise((resolve) => { answerOld = resolve; });
  });
  const routed = [];
  session.setUnauthorizedHandler((path) => routed.push(path));
  try {
    const oldRead = k8s.k8sFetch('GET', '/api/v1/nodes');
    await session.logout();
    await session.login('new-token');
    answerOld(new Response('expired', { status: 401 }));
    assert.equal((await oldRead).status, 401, 'the caller still receives its request result');
    assert.deepEqual(routed, [], 'an earlier session must not route the new session to login');
    assert.equal(session.currentIdentity().username, 'new');
    assert.equal(session.getCsrfToken(), 'new-csrf');
  } finally { session.setUnauthorizedHandler(null); }
});

test('delayed exec probes cannot overwrite a new identity or its CSRF token', async (t) => {
  let answerOld;
  t.mock.method(globalThis, 'fetch', async (url) => {
    if (String(url) === '/api/v1/logout') return new Response(null, { status: 204 });
    if (String(url) === '/api/v1/login') return Response.json({ user: { username: 'new' }, csrfToken: 'new-csrf' });
    return new Promise((resolve) => { answerOld = resolve; });
  });
  for (const status of [401, 200]) {
    // The old cookie is known here; the deliberately held /me is a probe,
    // not the additional CSRF recovery request needed after expiry.
    session.setCsrfToken('old-csrf');
    const oldProbe = session.probeMe();
    await session.logout();
    await session.login('new-token');
    answerOld(Response.json({ username: 'old', csrfToken: 'old-csrf' }, { status }));
    await oldProbe;
    assert.equal(session.currentIdentity()?.username, 'new', `old ${status} must not replace the new identity`);
    assert.equal(session.getCsrfToken(), 'new-csrf', `old ${status} must not replace the new CSRF token`);
  }
});

// ---- the log stream and the exec terminal on the transport ----------------
// harness.mjs executes the real modules and their render bodies but keeps
// effects inert by design; the behaviors this task pins — the request the log
// stream makes, its deadline, its 401 routing, and the decision the exec
// WebSocket makes when it closes — live exactly in that effect code. These
// tests therefore run the two real JSX modules in a vm context of their own,
// one that HAS the platform pieces that code touches (AbortController, the
// global timers the deadline rides, TextDecoder) and a fetch stub of its own.
// The components delegate to two seams the tests drive directly:
// streamPodLogs, the log stream's request-and-read runner, and
// execSessionEnded, the WebSocket's close-time decision.
const stream = {
  config: null,   // body served for /api/v1/config; null → 502
  me: null,       // body served for /api/v1/me; 'unauthorized' → 401
  calls: [],      // every other request, in order
  answer: null,   // what a Kubernetes call gets: { ok, status, chunks }
  hang: false,    // requests never answer, and end only by abort
  reject: null,
  defer: null,
};
const streamAbortError = () => { const e = new Error('the request was aborted'); e.name = 'AbortError'; return e; };
const streamFetch = async (url, opts = {}) => {
  const path = String(url);
  if (stream.defer) {
    const deferred = stream.defer;
    stream.defer = null;
    return new Promise(deferred);
  }
  if (path === '/api/v1/config') {
    return { ok: !!stream.config, status: stream.config ? 200 : 502, json: async () => stream.config };
  }
  if (path === '/api/v1/me') {
    if (stream.me === 'unauthorized') return { ok: false, status: 401, text: async () => '', json: async () => null };
    if (stream.me === 'network') throw new Error('connection lost');
    if (stream.me === 'server-error') return { ok: false, status: 500, text: async () => 'boom' };
    return { ok: true, status: 200, text: async () => JSON.stringify(stream.me), json: async () => stream.me };
  }
  stream.calls.push({ url: path, init: opts });
  if (stream.reject) { const e = stream.reject; stream.reject = null; throw e; }
  if (stream.hang) {
    return new Promise((resolve, reject) => {
      const signal = opts.signal;
      if (!signal) return;                    // no signal: nothing can end this request
      if (signal.aborted) return reject(streamAbortError());
      signal.addEventListener('abort', () => reject(streamAbortError()), { once: true });
    });
  }
  const a = stream.answer || { ok: true, status: 200, chunks: [] };
  const chunks = [...(a.chunks || [])];
  return {
    ok: a.ok, status: a.status,
    text: async () => chunks.join(''),
    json: async () => null,
    // A streamed answer hands its body over through a reader that ends when
    // the chunks run out — or stays open while the request's signal lives,
    // which is what a follow stream does.
    body: {
      getReader: () => ({
        read: () => {
          if (chunks.length) return Promise.resolve({ value: new TextEncoder().encode(chunks.shift()), done: false });
          if (a.end) return Promise.resolve({ value: undefined, done: true });
          const signal = opts.signal;
          if (!signal) return Promise.resolve({ value: undefined, done: true });
          return new Promise((resolve, reject) => {
            if (signal.aborted) return reject(streamAbortError());
            signal.addEventListener('abort', () => reject(streamAbortError()), { once: true });
          });
        },
      }),
    },
  };
};
const streamContext = vm.createContext({
  console: { warn() {}, log() {}, error() {} },
  URLSearchParams, TextDecoder, TextEncoder, AbortController,
  // The deadline rides the mockable global timer, so the vm realm must read
  // the test realm's setTimeout when the call is made, never a captured copy.
  setTimeout: (...a) => globalThis.setTimeout(...a),
  clearTimeout: (...a) => globalThis.clearTimeout(...a),
  fetch: streamFetch,
  window: {},
  __jsx: (type, props, ...children) => ({ type, props: { ...props, children } }),
  __fragment: 'fragment',
  location: { protocol: 'https:', host: 'dash.local' },
});
const streamHooks = {
  useState: (initial) => [typeof initial === 'function' ? initial() : initial, () => {}],
  useEffect: (fn) => stream.mountEffect?.(fn), useRef: () => ({ current: null }), useCallback: (fn) => fn,
  useMemo: (fn) => fn(), useSyncExternalStore: (_subscribe, snapshot) => snapshot(),
};
const streamCache = new Map();
async function streamModule(relative) {
  if (streamCache.has(relative)) return streamCache.get(relative);
  let code = await fs.readFile(new URL('../src/' + relative, import.meta.url), 'utf8');
  if (relative.endsWith('.jsx')) {
    ({ code } = await transformWithOxc(code, relative, { jsx: { runtime: 'classic', pragma: '__jsx', pragmaFrag: '__fragment' } }));
  }
  const mod = new vm.SourceTextModule(code, { context: streamContext, identifier: relative });
  streamCache.set(relative, mod);
  const stub = (values) => new vm.SyntheticModule(Object.keys(values), function () {
    for (const [name, value] of Object.entries(values)) this.setExport(name, value);
  }, { context: streamContext });
  await mod.link(async (specifier) => {
    // More specific suffixes first: the ui controls and the shared ones differ.
    for (const [match, make] of [
      ['@xterm/xterm/css/xterm.css', () => stub({})],
      ['@xterm/xterm', () => stub({ Terminal: class {
        constructor() { stream.term = this; this.lines = []; this.cols = 80; this.rows = 24; }
        loadAddon() {} open() {} focus() {} dispose() {}
        write(line) { this.lines.push(line); }
        onData(fn) { this.input = fn; return { dispose() {} }; }
        onResize(fn) { this.resize = fn; return { dispose() {} }; }
      } })],
      ['@xterm/addon-fit', () => stub({ FitAddon: class { fit() {} } })],
      ['../ui/controls.jsx', () => stub({ Button: 'Button', StatusBadge: 'StatusBadge', Select: 'Select', TextInput: 'TextInput' })],
      ['controls.jsx', () => stub({ Button: 'Button', Select: 'Select', TextInput: 'TextInput' })],
      ['../ui/shell.jsx', () => stub({ Modal: 'Modal' })],
      ['icons.jsx', () => stub({ Icon: 'Icon' })],
      ['terminal.jsx', () => stub({ ExecTerminal: 'ExecTerminal', NodeShell: 'NodeShell' })],
      ['node-shell.js', () => stub({ NODE_SHELL_NS: 'stratum-node-shell', startNodeShell: () => () => {} })],
      ['cluster-context.js', () => streamModule('cluster-context.js')],
      ['session.js', () => streamModule('session.js')],
      ['data.js', () => streamModule('data.js')],
      ['react', () => stub(streamHooks)],
    ]) {
      if (specifier === match || specifier.endsWith(match)) return make();
    }
    throw new Error('Unmocked dependency in the stream loader: ' + specifier);
  });
  await mod.evaluate();
  return mod;
}
const streamConfig = (await streamModule('cluster-context.js')).namespace;
const streamSession = (await streamModule('session.js')).namespace;
const panel = (await streamModule('ui/exec-logs-drawer.jsx')).namespace;
const terminal = (await streamModule('screens/terminal.jsx')).namespace;

test("the log stream fetches through the transport: the per-request prefix, a GET's headers, and the stream it hands back", async () => {
  const run = () => panel.streamPodLogs({
    ns: 'app', pod: 'demo', container: 'main', timestamps: true, previous: false, follow: true,
    signal: new AbortController().signal,
    onLine: (line) => lines.push(line),
  });
  const lines = [];

  stream.config = { mode: 'standalone', k8sBase: '/clusters/a/k8s', auth: { required: true } };
  await streamConfig.getConfig({ force: true });
  streamSession.setCsrfToken('csrf-logs');
  stream.answer = { ok: true, status: 200, chunks: ['line one\n', 'line two\n'], end: true };
  try {
    await run();
    assert.equal(stream.calls.length, 1, 'the stream must make exactly one request');
    const { url, init } = stream.calls[0];
    assert.equal(url.startsWith('/clusters/a/k8s/api/v1/namespaces/app/pods/demo/log?'), true, `${url} left the configured prefix`);
    assert.deepEqual(Object.fromEntries(new URL(url, 'https://dash.local').searchParams),
      { timestamps: 'true', tailLines: '1000', container: 'main', follow: 'true' },
      'the log query must carry exactly the panel\'s options');
    assert.equal(init.method, 'GET', 'the log stream is a read');
    assert.equal(init.headers.Accept, 'application/json', 'the request must carry the transport\'s Accept header');
    assert.equal(init.headers['X-Stratum-CSRF'], undefined, 'a GET must not carry the CSRF token, even when one is armed');
    assert.equal(init.credentials, 'same-origin', 'the request must be same-origin, like every transport call');
    assert.deepEqual(lines, ['line one', 'line two'], 'the streamed body must reach the pane as lines');

    // The prefix is read per request, never captured at module load: the same
    // call must follow the config when it names another base.
    stream.config = { mode: 'standalone', k8sBase: '/clusters/b/k8s', auth: { required: true } };
    await streamConfig.getConfig({ force: true });
    stream.calls.length = 0;
    await run();
    assert.equal(stream.calls[0].url.startsWith('/clusters/b/k8s/'), true, `${stream.calls[0].url} ignored the new prefix`);
  } finally { streamSession.setCsrfToken(null); stream.answer = null; }
});

test('a 401 on the log stream sends the user to sign-in, and a 403 does not', async () => {
  stream.config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true } };
  await streamConfig.getConfig({ force: true });
  const seen = [];
  streamSession.setUnauthorizedHandler((path) => seen.push(path));
  const run = () => panel.streamPodLogs({
    ns: 'app', pod: 'demo', follow: true,
    signal: new AbortController().signal,
    onLine: () => {},
  });
  try {
    // A 401 means the dashboard session ended, the same rule every other
    // Kubernetes call answers to, so the stream routes to the unauthorized
    // handler session.js holds — and still tells the pane it failed.
    stream.answer = { ok: false, status: 401, chunks: [] };
    await run();
    assert.equal(seen.length, 1, 'the 401 must route to the unauthorized handler exactly once');
    assert.equal(seen[0].startsWith('/api/v1/namespaces/app/pods/demo/log'), true, `${seen[0]} is not the log request's path`);

    // A 403 is RBAC refusing this user; the session is alive, so no hop.
    seen.length = 0;
    stream.answer = { ok: false, status: 403, chunks: [] };
    await run();
    assert.deepEqual(seen, [], 'a 403 must not send the user to sign-in');
  } finally { streamSession.setUnauthorizedHandler(null); stream.answer = null; }
});

test('log request failures are visible, while an intentional stop is quiet', async () => {
  const lines = [];
  const run = (signal) => panel.streamPodLogs({ ns: 'app', pod: 'demo', follow: true,
    signal, onLine: (line) => lines.push(line) });
  stream.reject = new Error('network unavailable');
  await run(new AbortController().signal);
  assert.ok(lines.some((line) => line.includes('network unavailable')));
  lines.length = 0;
  const stopped = new AbortController();
  stopped.abort();
  stream.reject = Object.assign(new Error('stopped'), { name: 'AbortError' });
  await run(stopped.signal);
  assert.deepEqual(lines, []);
});

test('a never-answering log fetch ends at its deadline, a stop abort still lands at once, and an open stream outlives the clock', { timeout: 2000 }, async (t) => {
  stream.calls.length = 0;
  stream.config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true } };
  await streamConfig.getConfig({ force: true });
  t.mock.timers.enable({ apis: ['setTimeout'] });
  try {
    // A proxy that accepts the request and then stalls must not hang the pane
    // past the deadline, and the deadline and the caller's stop signal must
    // flow through one composed signal, as they do for k8sFetch.
    stream.hang = true;
    const ctl = new AbortController();
    const timeoutLines = [];
    const stalled = panel.streamPodLogs({ ns: 'app', pod: 'demo', follow: true, signal: ctl.signal,
      onLine: (line) => timeoutLines.push(line) });
    t.mock.timers.tick(60_000);
    await stalled;
    assert.equal(stream.calls[0].init.signal.aborted, true, 'the deadline must reach the request through its signal');
    assert.notEqual(stream.calls[0].init.signal, ctl.signal, 'the deadline and the stop signal must be one composed signal');
    assert.ok(timeoutLines.some((line) => line.includes('deadline')), 'the timeout must be visible in the log pane');

    // The user's stop must land without waiting for the clock, even while a
    // deadline is armed.
    stream.calls.length = 0;
    const stop = new AbortController();
    const stopping = panel.streamPodLogs({ ns: 'app', pod: 'demo', follow: true, signal: stop.signal, onLine: () => {} });
    stop.abort();
    assert.notEqual(await Promise.race([stopping, new Promise((r) => realSetTimeout(r, 2000, 'stuck'))]),
      'stuck', 'a stop abort must reach the request without waiting for the deadline');
    assert.equal(stream.calls[0].init.signal.aborted, true, 'the stop abort must have aborted the request');

    // Once the response is in, the deadline is done: a follow stream stays
    // open past any clock and ends only when the user stops it or the pane
    // closes — a deadline over the whole body read would cut live logs off.
    stream.hang = false;
    stream.calls.length = 0;
    stream.answer = { ok: true, status: 200, chunks: ['hello\n'] };
    const open = new AbortController();
    const opened = panel.streamPodLogs({ ns: 'app', pod: 'demo', follow: true, signal: open.signal, onLine: () => {} });
    t.mock.timers.tick(60_000);
    assert.equal(stream.calls[0].init.signal.aborted, false, 'an open stream must not be cut off by the deadline');
    open.abort();
    await opened;
  } finally { stream.hang = false; stream.answer = null; }
});

test('a quiet log follow stream stays open until the caller stops it', { timeout: 2000 }, async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  stream.hang = false;
  stream.answer = { ok: true, status: 200, chunks: ['initial line\n'] };
  stream.calls.length = 0;
  const stop = new AbortController();
  const lines = [];
  let ended = false;
  const opened = panel.streamPodLogs({ ns: 'app', pod: 'quiet', follow: true,
    signal: stop.signal, onLine: (line) => lines.push(line) }).then(() => { ended = true; });
  try {
    // Let the response and initial chunk arrive before advancing the clock:
    // a healthy pod need not emit another line within the request deadline.
    for (let i = 0; i < 20; i++) await Promise.resolve();
    assert.deepEqual(lines, ['initial line']);
    t.mock.timers.tick(60_000);
    for (let i = 0; i < 20; i++) await Promise.resolve();
    assert.equal(stream.calls[0].init.signal.aborted, false, 'silence must not abort a follow stream');
    assert.equal(ended, false, 'following logs must continue across quiet periods');
  } finally {
    stop.abort();
    await opened;
    stream.answer = null;
  }
  assert.equal(ended, true, 'caller cancellation still ends the stream');
});

test('a finite log body stall is visible after response headers and partial lines arrive', { timeout: 2000 }, async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  stream.answer = { ok: true, status: 200, chunks: ['partial line\n'] };
  const lines = [];
  const stop = new AbortController();
  const running = panel.streamPodLogs({ ns: 'app', pod: 'demo', follow: false,
    signal: stop.signal, onLine: (line) => lines.push(line) });
  try {
    for (let i = 0; i < 20; i++) await Promise.resolve();
    assert.deepEqual(lines, ['partial line']);
    t.mock.timers.tick(15_000);
    await running;
    assert.ok(lines.some((line) => line.includes('[error]') && line.includes('deadline')),
      'a truncated response must report its deadline failure');
  } finally { stop.abort(); stream.answer = null; }
});

test('old exec and log responses cannot route a later session to login', async () => {
  const routed = [];
  streamSession.setUnauthorizedHandler((path) => routed.push(path));
  try {
    for (const kind of ['exec', 'logs']) {
      let answerOld;
      stream.defer = (resolve) => { answerOld = resolve; };
      const pending = kind === 'exec'
        ? terminal.execSessionEnded({ wasClean: false })
        : panel.streamPodLogs({ ns: 'app', pod: 'demo', follow: true, onLine: () => {} });
      await streamSession.logout();
      stream.me = { username: 'new', csrfToken: 'new-csrf' };
      await streamSession.fetchMe();
      answerOld({ ok: false, status: 401, text: async () => 'expired' });
      await pending;
      assert.deepEqual(routed, [], `${kind} response from an old session must not route the new one`);
      assert.equal(streamSession.currentIdentity()?.username, 'new');
      assert.equal(streamSession.getCsrfToken(), 'new-csrf');
    }
  } finally { stream.defer = null; stream.me = null; streamSession.setUnauthorizedHandler(null); }
});

test('an abnormal exec close routes only a confirmed /me 401; a clean close asks nothing', async () => {
  const asked = [], routed = [];
  const ask = (answer) => async () => (asked.push('me'), answer);
  const route = (path) => routed.push(path);

  // A clean close is the session or the pane ending on its own terms: no
  // question, no routing.
  assert.equal(await terminal.execSessionEnded({ wasClean: true, code: 1000 }, { probe: ask({ status: 200 }), route }),
    false, 'a clean close must not route');
  assert.deepEqual(asked, [], 'a clean close must not ask /me');
  assert.deepEqual(routed, []);

  // An abnormal close may be a dead session — the handshake status is
  // invisible to the browser — but only /me can tell, quietly. An identity
  // answer means the session is alive.
  asked.length = 0; routed.length = 0;
  assert.equal(await terminal.execSessionEnded({ wasClean: false, code: 1006 }, { probe: ask({ status: 200 }), route }),
    false, 'a live session must not route');
  assert.deepEqual(asked, ['me'], 'an abnormal close must ask /me');
  assert.deepEqual(routed, []);

  // Only a confirmed 401 means the session ended; transient failures do not.
  asked.length = 0;
  assert.equal(await terminal.execSessionEnded({ wasClean: false, code: 1006 }, { probe: ask({ status: 401 }), route }),
    true, 'a 401 /me answer must route');
  assert.deepEqual(routed, ['/api/v1/me']);
  for (const status of [0, 500]) {
    routed.length = 0;
    assert.equal(await terminal.execSessionEnded({ wasClean: false, code: 1006 }, { probe: ask({ status }), route }), false);
    assert.deepEqual(routed, [], `${status} must not route`);
  }
});

test('an old exec socket closing after relogin neither probes nor routes the new session', async () => {
  const revision = streamSession.currentSessionRevision();
  await streamSession.logout();
  stream.me = { username: 'new', csrfToken: 'new-csrf' };
  await streamSession.fetchMe();
  const asked = [], routed = [];
  try {
    const result = await terminal.execSessionEnded({ wasClean: false }, {
      revision, probe: async () => { asked.push('me'); return { status: 401 }; },
      route: (path) => routed.push(path),
    });
    assert.equal(result, false);
    assert.deepEqual(asked, [], 'a superseded socket must not check the new login');
    assert.deepEqual(routed, []);
  } finally { stream.me = null; }
});

test('the exec close decision runs on the real /me probe and routes through the session handler', async () => {
  stream.config = { mode: 'standalone', k8sBase: '/k8s', auth: { required: true } };
  await streamConfig.getConfig({ force: true });
  const routed = [];
  streamSession.setUnauthorizedHandler((path) => routed.push(path));
  try {
    // /me answers 401: the real probe reports it, and the close decision
    // routes the current session to sign-in.
    stream.me = 'unauthorized';
    await terminal.execSessionEnded({ wasClean: false, code: 1006 });
    assert.deepEqual(routed, ['/api/v1/me'], 'a real 401 from /me must end in a sign-in route');

    // /me answers with the signed-in identity: the session is alive, no route.
    routed.length = 0;
    stream.me = { username: 'op', provider: 'kubernetes', groups: [] };
    await terminal.execSessionEnded({ wasClean: false, code: 1006 });
    assert.deepEqual(routed, [], 'a real identity from /me must not route');
    for (const failure of ['server-error', 'network']) {
      stream.me = failure;
      await terminal.execSessionEnded({ wasClean: false, code: 1006 });
      assert.deepEqual(routed, [], `${failure} must not route`);
    }
  } finally { streamSession.setUnauthorizedHandler(null); stream.me = null; }
});

test('delayed log headers and chunks cannot expose the previous tab session', async () => {
  for (const phase of ['headers', 'chunk']) {
    const storage = { value: null, getItem() { return this.value; }, setItem(_key, value) { this.value = value; } };
    streamContext.window.localStorage = storage;
    let release, fetchedSignal, timer;
    const lines = [];
    const response = { ok: true, status: 200, body: { getReader: () => ({
      read: () => new Promise((resolve) => { release = resolve; }),
    }) } };
    const original = streamContext.fetch;
    streamContext.fetch = async (_url, opts) => {
      fetchedSignal = opts.signal;
      return phase === 'headers' ? new Promise((resolve) => { release = resolve; }) : response;
    };
    const pending = panel.streamPodLogs({ ns: 'app', pod: 'private-a', follow: true, onLine: (line) => lines.push(line) });
    try {
      for (let i = 0; i < 20; i++) await Promise.resolve();
      storage.value = JSON.stringify({ generation: 'user-b', changing: false });
      release(phase === 'headers' ? response : { done: false, value: new TextEncoder().encode('private-a-line\n') });
      await Promise.race([pending, new Promise((_, reject) => {
        timer = realSetTimeout(() => reject(new Error('obsolete log stream did not stop')), 1000);
      })]);
      assert.deepEqual(lines, [], `${phase} from A cannot reach B's pane`);
      assert.equal(fetchedSignal.aborted, true);
    } finally {
      release?.(phase === 'headers' ? response : { done: true });
      // Closing a test stream remains available even if a regression fails to fence it.
      clearTimeout(timer);
      release?.({ done: true });
      streamContext.fetch = original;
      delete streamContext.window.localStorage;
    }
  }
});


test('an exec socket cannot send input or display output after another tab changes the cookie', () => {
  for (const operation of ['output', 'input', 'resize', 'open']) {
    const storage = { value: null, getItem() { return this.value; } };
    streamContext.window.localStorage = storage;
    const sockets = [];
    streamContext.WebSocket = class {
      constructor() { sockets.push(this); this.readyState = 1; this.sent = []; }
      send(value) { this.sent.push(value); }
      close() { this.readyState = 3; }
    };
    streamContext.ResizeObserver = class { observe() {} disconnect() {} };
    let cleanup;
    stream.mountEffect = (fn) => { cleanup = fn(); };
    try {
      terminal.ExecTerminal({ ns: 'app', pod: 'private-a' });
      const ws = sockets[0], term = stream.term;
      ws.onopen();
      const sent = ws.sent.length, lines = term.lines.length;
      storage.value = JSON.stringify({ generation: `replacement-${operation}`, changing: false });
      if (operation === 'output') ws.onmessage({ data: new Uint8Array([1, ...new TextEncoder().encode('private-a-output')]).buffer });
      if (operation === 'input') term.input('rm -rf /old-user-data');
      if (operation === 'resize') term.resize({ cols: 100, rows: 30 });
      if (operation === 'open') ws.onopen();
      assert.equal(ws.sent.length, sent, 'no frames may use the old authenticated socket');
      assert.equal(term.lines.length, lines, 'no old private output may reach the terminal');
      assert.equal(ws.readyState, 3, 'the obsolete socket must close');
    } finally {
      cleanup?.();
      stream.mountEffect = null;
      delete streamContext.WebSocket; delete streamContext.ResizeObserver;
      delete streamContext.window.localStorage;
    }
  }
});
