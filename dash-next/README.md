# dash-next

The unified Stratum dashboard: one React app for the single-cluster dashboard server and the
fleet hub. Conventions are in [AGENTS.md](AGENTS.md); behavior is specified in `../spec`.

## Run against the fleet mock

```sh
node ../../stratum-pro/stratum-dash/mock/fleet-mock.mjs   # the hub mock, http://127.0.0.1:8443
DASH_BACKEND=http://127.0.0.1:8443 pnpm dev               # the app, http://localhost:7750
```

Sign in as `admin` / `admin` (read-only: `viewer` / `viewer`). The dev server proxies `/api`
and `/k8s` to `DASH_BACKEND` (default `http://127.0.0.1:8443`); point it at a real dashboard
server or hub the same way. The mock forwards Kubernetes calls to `kubectl proxy` on port 8001.

## Run against a real Stratum, beside the current dashboard

```sh
pnpm dev:cluster
```

One command: it starts a single-node cluster in a Docker container (the way the integration
tests run a node), waits for its dashboard server, reads the admin token the cluster
generated, and starts the dev server on `http://localhost:7750`, which opens already signed
in. `http://localhost:7750/dev-login` signs in again after a sign-out or an expired session;
the route exists only in the dev server.

The first run creates the node and needs a Linux `stratum` binary for Docker's architecture
at `dist/stratum` in the repository root (`make package`, or a release asset):

```sh
ARCH=$(docker version --format '{{.Server.Arch}}')
gh release download <tag> -R ctrliq/stratum -p "stratum-linux-$ARCH" -O dist/stratum && chmod +x dist/stratum
```

Later runs reuse the node, starting it if it is stopped. It keeps running when the dev server
stops: `docker rm -f stratum-dev` stops it, the `stratum-dev` volume keeps the cluster, and
`docker volume rm stratum-dev` discards it.

| Dashboard | URL | Served by |
| --- | --- | --- |
| current (`dash`) | `https://127.0.0.1:30443` | the cluster's own dashboard, as the binary ships it |
| `dash-next` | `http://localhost:7750` | the dev server, proxying to a second dashboard server on port 8443 |

The second server exists because the dashboard server refuses a browser whose `Origin` is not
its own, and it is started with the dev server's as its public URL; that is also why
`dev:cluster` refuses to start on any port but 7750. Open the two on different hosts as above
(`127.0.0.1` and `localhost`): the session cookie has one name and cookies ignore the port, so
on one host each sign-in would replace the other's. The current dashboard still takes the
token by hand:

```sh
docker exec stratum-dev /var/lib/stratum/bin/kubectl --kubeconfig /var/lib/stratum/kubeconfig/admin.conf \
  -n stratum-dashboard get secret stratum-dashboard-admin-token -o jsonpath='{.data.token}' | base64 -d; echo
```

## How a hub embeds it

A fleet hub is built outside this repository and may not link this module's packages
into its binary, so it takes the dashboard as a file. `make dashboard-asset` (from the
repository root) writes `dist/stratum-dashboard.tar.gz`:

| Entry | What | Read by |
| --- | --- | --- |
| `dist/` | the built SPA | the hub's embed |
| `packages.json` | the npm packages bundled into it | the hub's license check |
| `fleet.openapi.yaml` | the hub API the client was generated from | the hub's route check |

The release workflow signs the archive and pushes it to
`ghcr.io/ctrliq/stratum:<tag>-dashboard`, beside the other importer assets. A hub pinned to
that tag embeds exactly those bytes, so a change to `openapi/fleet.openapi.yaml` reaches a
hub with the release that carries it, and that hub's build fails if it does not serve a
route the contract names.

## Checks

```sh
pnpm typecheck && pnpm exec eslint . && pnpm lint:css && pnpm test && pnpm build
```

## Adding a screen

Screens are listed in `src/domain/shell/constants.ts` (`screens`). To build one, add a route
file `src/routes/c.$cluster.<segment>.tsx` that renders the domain's page; the shell supplies
the frame, `ClusterContext`, `NamespaceContext`, `WriteAccessContext` and `LiveRefreshContext`.
A fleet page (`/clusters`, `/operations`) is a top-level route that wraps its page in
`FleetShell`.
