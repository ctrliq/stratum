import type { ServerResponse } from 'node:http';

import type { Plugin } from 'vite';

export const devLoginPath = '/dev-login';

const noToken = 'No sign-in token: start the dev server with `pnpm dev:cluster`.';

type SignIn = {
  isAccepted: boolean;
  cookies: string[];
  reason: string;
};

const toSignIn = async (backend: string, token: string): Promise<SignIn> => {
  const answer = await fetch(`${backend}/api/v1/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ token }),
  });
  return {
    isAccepted: answer.ok,
    cookies: answer.headers.getSetCookie(),
    reason: answer.ok ? '' : await answer.text(),
  };
};

const withRefusal = (response: ServerResponse, reason: string) =>
  response.writeHead(502, { 'content-type': 'text/plain' }).end(reason);

const withSession = (response: ServerResponse, cookies: string[]) =>
  response.writeHead(302, { 'set-cookie': cookies, location: '/' }).end();

export const devLogin = (backend: string, token: string | undefined): Plugin => ({
  name: 'stratum:dev-login',
  apply: 'serve',
  config: () => (token ? { server: { open: devLoginPath } } : {}),
  configureServer: (server) => {
    server.middlewares.use(devLoginPath, (_request, response) => {
      void (
        token
          ? toSignIn(backend, token).then(({ isAccepted, cookies, reason }) =>
              isAccepted ? withSession(response, cookies) : withRefusal(response, reason),
            )
          : Promise.resolve(withRefusal(response, noToken))
      ).catch((error: unknown) => withRefusal(response, String(error)));
    });
  },
});
