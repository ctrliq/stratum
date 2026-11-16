# networking: trace

Scenario → status → implementing files, written by `implement-spec`. Paths are relative to `~/Code/stratum`. `dropped` rows carry `@drop` in the feature file.

## kinds.feature

| Scenario | Status | Implemented in |
|---|---|---|
| Each networking kind has its own list | implemented | dash-next/src/domain/networking/constants.ts, dash-next/src/domain/networking/pages/*List/index.tsx, dash-next/src/routes/c.$cluster.{services,endpoints,endpointslices,ingress,ingresses,ingressclasses,networkpolicies}.tsx |
| The sidebar's Network section lists the six kinds | implemented | dash-next/src/domain/shell/constants.ts (screens) |
| Each networking kind shows its columns in order | implemented | dash-next/src/domain/networking/constants.ts, dash-next/src/domain/resources/utils/transforms.ts (withIdentityColumns) |
| A networking column shows a value read from the object | implemented | dash-next/src/domain/networking/utils/transforms.ts |
| Service Type calls out headless services | implemented | dash-next/src/domain/networking/utils/transforms.ts (toServiceRow) |
| Service Ports list port, node port and protocol | implemented | dash-next/src/domain/networking/utils/formatters.ts (toServicePortText), dash-next/src/domain/networking/utils/transforms.ts (toServiceRow) |
| A service's external address and target ports appear only in its manifest | implemented | dash-next/src/domain/networking/constants.ts (serviceColumns), dash-next/src/domain/resources/components/ResourceBrowser.tsx |
| Endpoints Addresses shows at most four ready addresses | implemented | dash-next/src/domain/networking/utils/selectors.ts (readyAddressesOf), dash-next/src/domain/networking/utils/transforms.ts (toEndpointsRow) |
| Endpoint slice Ports list port and protocol | implemented | dash-next/src/domain/networking/utils/formatters.ts (toSlicePortText), dash-next/src/domain/networking/utils/transforms.ts (toEndpointSliceRow) |
| Ingress Hosts lists the rule hosts | implemented | dash-next/src/domain/networking/utils/selectors.ts (hostsOf), dash-next/src/domain/networking/utils/transforms.ts (toIngressRow) |
| The sidebar's Ingresses row uses the route "ingress" | implemented | dash-next/src/domain/shell/constants.ts (screens), dash-next/src/routes/c.$cluster.ingress.tsx, dash-next/src/routes/c.$cluster.ingresses.tsx, dash-next/src/domain/networking/pages/IngressesList/index.tsx |
| Only the route "ingress" lights the sidebar row | dropped | D28 |
| The breadcrumb for the ingress route reads Ingresses | implemented | dash-next/src/domain/shell/constants.ts, dash-next/src/domain/shell/utils/transforms.ts (toCrumbs) |
| The breadcrumb for the ingress route reads Ingress | dropped | — |
| Networking kinds have no actions of their own | implemented | dash-next/src/domain/networking/pages/*List/index.tsx (no actions, scale or restart handed to ResourceBrowser) |
| Endpoint slices can be created, edited and deleted | implemented | dash-next/src/domain/networking/pages/EndpointSlicesList/index.tsx |
| Endpoint slices are read-only | implemented | dash-next/src/domain/networking/pages/EndpointSlicesList/index.tsx (isReadOnly off the dashboard server), dash-next/src/domain/auth/utils/predicates.ts (isDashboardServer) |
