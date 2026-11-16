#!/usr/bin/env node
import { execFileSync, spawn } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { setTimeout as later } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

const container = 'stratum-dev';
const image = 'ske-inttest-harness:local';
const backend = 'http://127.0.0.1:8443';
const acceptedPort = '7750';
const originTheNodeAccepts = `http://localhost:${acceptedPort}`;
const kubeconfig = '/var/lib/stratum/kubeconfig/admin.conf';
const kubectl = '/var/lib/stratum/bin/kubectl';
const pollMs = 3000;
const startupAttempts = 200;

const app = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repo = resolve(app, '..');
const binary = resolve(repo, 'dist/stratum');

const say = (text) => console.error(`dev-cluster: ${text}`);
const docker = (...args) =>
  execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
const attempted = (read) => {
  try {
    return read();
  } catch {
    return undefined;
  }
};

const stateOf = () =>
  ({ true: 'running', false: 'stopped' })[
    attempted(() => docker('inspect', '--format', '{{.State.Running}}', container))
  ] ?? 'missing';

const alpineVersion = () =>
  /^alpine_version *= *(\S+)/m.exec(readFileSync(resolve(repo, 'build/versions.mk'), 'utf8'))[1];

const nodeCommand = `. /inttest/harness.sh; setup_cgroups
(until /dist/stratum dash --listen 0.0.0.0:8443 --insecure-http --public-url ${originTheNodeAccepts} \
  --kubeconfig ${kubeconfig}; do sleep 3; done) &
exec /dist/stratum up`;

const created = () => {
  if (!existsSync(binary)) {
    throw new Error(
      `${binary} is missing. Stage a Linux stratum binary for Docker's architecture there: ` +
        '`make package`, or a release asset (see README.md).',
    );
  }
  say('building the node image');
  execFileSync(
    'docker',
    ['build', '-t', image, '--build-arg', `ALPINE_VERSION=${alpineVersion()}`, 'tools/inttest/'],
    { cwd: repo, stdio: 'inherit' },
  );
  say('creating the node (first boot takes a few minutes)');
  docker(
    ...['run', '-d', '--privileged', '--cgroupns=private'],
    ...['--name', container, '--hostname', container],
    ...['-p', '127.0.0.1:30443:30443', '-p', '127.0.0.1:6443:6443', '-p', '127.0.0.1:8443:8443'],
    ...['-v', `${resolve(repo, 'dist')}:/dist:ro`],
    ...['-v', `${resolve(repo, 'tools/inttest')}:/inttest:ro`],
    ...['-v', `${container}:/var/lib/stratum`],
    ...['--tmpfs', '/run:exec,mode=755', '--entrypoint', 'bash', image, '-c', nodeCommand],
  );
};

const started = () => {
  say('starting the stopped node');
  docker('start', container);
};

const isServing = () =>
  fetch(`${backend}/api/v1/config`).then(
    ({ ok }) => ok,
    () => false,
  );

const tokenOf = () =>
  attempted(() =>
    Buffer.from(
      docker(
        ...['exec', container, kubectl, '--kubeconfig', kubeconfig, '-n', 'stratum-dashboard'],
        ...['get', 'secret', 'stratum-dashboard-admin-token', '-o', 'jsonpath={.data.token}'],
      ),
      'base64',
    ).toString(),
  );

const awaited = async (what, read, attempts = startupAttempts) => {
  const value = await read();
  if (value) {
    return value;
  }
  if (attempts === 0) {
    throw new Error(`gave up waiting for ${what}; see \`docker logs ${container}\``);
  }
  if (attempts % 10 === 0) {
    say(`waiting for ${what}`);
  }
  await later(pollMs);
  return awaited(what, read, attempts - 1);
};

const main = async () => {
  ({ missing: created, stopped: started, running: () => {} })[stateOf()]();
  await awaited('the dashboard server', isServing);
  const token = await awaited('the admin token', tokenOf);
  say(
    `signed-in dev server at ${originTheNodeAccepts} (sign in again at ${originTheNodeAccepts}/dev-login)`,
  );
  spawn('pnpm', ['exec', 'vite', '--port', acceptedPort, '--strictPort'], {
    cwd: app,
    stdio: 'inherit',
    env: { ...process.env, DASH_BACKEND: backend, DASH_DEV_TOKEN: token },
  }).on('exit', (code) => process.exit(code ?? 0));
};

main().catch((error) => {
  say(error.message);
  process.exit(1);
});
