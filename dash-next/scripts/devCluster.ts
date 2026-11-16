import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import type { ServerResponse } from 'node:http';
import { resolve } from 'node:path';
import { setTimeout as later } from 'node:timers/promises';

import type { Plugin } from 'vite';

const container = 'stratum-dev';
const image = 'ske-inttest-harness:local';
const backend = 'http://127.0.0.1:8443';
const acceptedPort = 7750;
const originTheNodeAccepts = `http://localhost:${acceptedPort}`;
const signInPath = '/dev-login';
const kubeconfig = '/var/lib/stratum/kubeconfig/admin.conf';
const kubectl = '/var/lib/stratum/bin/kubectl';
const pollMs = 3000;
const startupAttempts = 200;
const attemptsPerNotice = 10;

const repo = resolve(process.cwd(), '..');
const binary = resolve(repo, 'dist/stratum');

const nodeCommand = `. /inttest/harness.sh; setup_cgroups
(until /dist/stratum dash --listen 0.0.0.0:8443 --insecure-http --public-url ${originTheNodeAccepts} \
  --kubeconfig ${kubeconfig}; do sleep 3; done) &
exec /dist/stratum up`;

const containerStates: Record<string, 'running' | 'stopped'> = {
  true: 'running',
  false: 'stopped',
};

const say = (text: string) => console.error(`dev-cluster: ${text}`);

const docker = (...args: string[]) =>
  execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();

const attempted = <T>(read: () => T): T | undefined => {
  try {
    return read();
  } catch {
    return undefined;
  }
};

const stateOf = () =>
  containerStates[
    attempted(() => docker('inspect', '--format', '{{.State.Running}}', container)) ?? ''
  ] ?? 'missing';

const alpineVersion = () =>
  /^alpine_version *= *(\S+)/m.exec(readFileSync(resolve(repo, 'build/versions.mk'), 'utf8'))?.[1];

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

const nodeStarts = { missing: created, stopped: started, running: () => undefined };

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

const awaited = async (
  what: string,
  read: () => unknown,
  attempts = startupAttempts,
): Promise<void> => {
  const value = await read();
  if (attempts % attemptsPerNotice === 0 && !value) {
    say(`waiting for ${what}`);
  }
  return value
    ? undefined
    : attempts === 0
      ? Promise.reject(new Error(`gave up waiting for ${what}; see \`docker logs ${container}\``))
      : later(pollMs).then(() => awaited(what, read, attempts - 1));
};

const toSessionCookies = async (token: string): Promise<string[]> => {
  const answer = await fetch(`${backend}/api/v1/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ token }),
  });
  return answer.ok ? answer.headers.getSetCookie() : Promise.reject(new Error(await answer.text()));
};

const withRefusal = (response: ServerResponse, reason: string) =>
  response.writeHead(502, { 'content-type': 'text/plain' }).end(reason);

const withSession = (response: ServerResponse, cookies: string[]) =>
  response.writeHead(302, { 'set-cookie': cookies, location: '/' }).end();

export const devCluster = (): Plugin => ({
  name: 'stratum:dev-cluster',
  apply: (_config, { command, mode }) => command === 'serve' && mode === 'cluster',
  config: async () => {
    nodeStarts[stateOf()]();
    await awaited('the dashboard server', isServing);
    await awaited('the admin token', tokenOf);
    return { server: { port: acceptedPort, strictPort: true, open: signInPath } };
  },
  configureServer: (server) => {
    server.middlewares.use(signInPath, (_request, response) => {
      void toSessionCookies(tokenOf() ?? '').then(
        (cookies) => withSession(response, cookies),
        (error: unknown) => withRefusal(response, String(error)),
      );
    });
  },
});
