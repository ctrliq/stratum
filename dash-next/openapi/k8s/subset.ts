/**
 * @file Builds k8s.subset.json: the slice of the upstream Kubernetes OpenAPI
 * documents the dashboard calls, plus the Stratum CRDs. Run it when the
 * pinned tag or the allow-list changes; the output is committed.
 */
import { readFile, writeFile } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { parseAllDocuments } from 'yaml';

import {
  allowedGroupVersions,
  type AllowedResources,
  crdManifests,
  droppedQueryParameters,
  versionsManifest,
} from './allowlist.ts';

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
type JsonObject = { [key: string]: Json };
type Schemas = Record<string, JsonObject>;
type Slice = { paths: JsonObject; schemas: Schemas };

const here = dirname(fileURLToPath(import.meta.url));
const kubernetesTag = `v${
  /^kubernetes_version\s*=\s*(\S+)/m.exec(
    await readFile(resolve(here, versionsManifest), 'utf8'),
  )?.[1] ?? ''
}`;
const schemaRefPrefix = '#/components/schemas/';
const httpMethods = ['get', 'put', 'post', 'patch', 'delete'] as const;
const jsonContent = 'application/json';
const preferredContent = [jsonContent, 'application/merge-patch+json', 'text/plain'] as const;

const isObject = (value: Json | undefined): value is JsonObject =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

const mapJson = (value: Json, withNode: (node: JsonObject) => JsonObject): Json =>
  Array.isArray(value)
    ? value.map((item) => mapJson(item, withNode))
    : isObject(value)
      ? Object.fromEntries(
          Object.entries(withNode(value)).map(([key, child]) => [key, mapJson(child, withNode)]),
        )
      : value;

const refsIn = (value: Json): string[] =>
  Array.isArray(value)
    ? value.flatMap(refsIn)
    : isObject(value)
      ? Object.entries(value).flatMap(([key, child]) =>
          key === '$ref' && typeof child === 'string'
            ? [child.slice(schemaRefPrefix.length)]
            : refsIn(child),
        )
      : [];

const reachableFrom = (
  names: string[],
  schemas: Schemas,
  seen: ReadonlySet<string> = new Set(),
): ReadonlySet<string> => {
  const fresh = [...new Set(names)].filter((name) => !seen.has(name) && name in schemas);
  return fresh.length === 0
    ? seen
    : reachableFrom(
        fresh.flatMap((name) => refsIn(schemas[name])),
        schemas,
        new Set([...seen, ...fresh]),
      );
};

const sourceUrlFor = (groupVersion: string): string =>
  `https://raw.githubusercontent.com/kubernetes/kubernetes/${kubernetesTag}/api/openapi-spec/v3/${groupVersion.replaceAll('/', '__')}_openapi.json`;

const fromUpstream = async (groupVersion: string): Promise<JsonObject> => {
  const response = await fetch(sourceUrlFor(groupVersion));
  return response.ok
    ? ((await response.json()) as JsonObject)
    : Promise.reject(new Error(`${sourceUrlFor(groupVersion)}: HTTP ${response.status}`));
};

const resourceAt = (
  groupVersion: string,
  path: string,
): { resource: string; subresource: string | undefined } => {
  const segments = path.slice(`/${groupVersion}/`.length).split('/');
  const isNamespaced =
    segments[0] === 'namespaces' && segments[1] === '{namespace}' && segments.length > 2;
  const [resource, , subresource] = isNamespaced ? segments.slice(2) : segments;
  return { resource, subresource };
};

const isAllowedPath =
  (groupVersion: string, allowed: AllowedResources) =>
  (path: string): boolean => {
    const { resource, subresource } = resourceAt(groupVersion, path);
    return (
      resource in allowed && (subresource === undefined || allowed[resource].includes(subresource))
    );
  };

const isKeptParameter = (parameter: Json): boolean =>
  !(isObject(parameter) && droppedQueryParameters.some((name) => name === parameter.name));

const withKeptParameters = (node: JsonObject): JsonObject =>
  Array.isArray(node.parameters)
    ? { ...node, parameters: node.parameters.filter(isKeptParameter) }
    : node;

const withSingleContent = (node: JsonObject): JsonObject => {
  const content = isObject(node.content) ? node.content : undefined;
  const chosen = content
    ? (preferredContent.find((type) => type in content) ?? Object.keys(content)[0])
    : undefined;
  return content && chosen
    ? {
        ...node,
        content: {
          [chosen === 'text/plain' ? chosen : jsonContent]: content[chosen],
        },
      }
    : node;
};

const isCollectionDelete = ([method, operation]: [string, Json]): boolean =>
  method === 'delete' &&
  isObject(operation) &&
  /^delete\w*Collection/.test(String(operation.operationId));

const toSlimPathItem = (pathItem: Json): Json =>
  isObject(pathItem)
    ? mapJson(
        Object.fromEntries(Object.entries(pathItem).filter((entry) => !isCollectionDelete(entry))),
        (node) => withSingleContent(withKeptParameters(node)),
      )
    : pathItem;

const toSlice = (groupVersion: string, allowed: AllowedResources, document: JsonObject): Slice => {
  const paths = isObject(document.paths) ? document.paths : {};
  const components = isObject(document.components) ? document.components : {};
  return {
    paths: Object.fromEntries(
      Object.entries(paths)
        .filter(([path]) => isAllowedPath(groupVersion, allowed)(path))
        .map(([path, pathItem]) => [path, toSlimPathItem(pathItem)]),
    ),
    schemas: (isObject(components.schemas) ? components.schemas : {}) as Schemas,
  };
};

const toPascal = (text: string): string =>
  text
    .split(/[^a-zA-Z0-9]+/)
    .filter(Boolean)
    .map((word) => word[0].toUpperCase() + word.slice(1))
    .join('');

const shortNameOf = (name: string): string => name.split('.').at(-1) ?? name;

const qualifiedNameOf = (name: string): string => name.split('.').slice(-3).map(toPascal).join('');

const toRenames = (names: string[]): Record<string, string> => {
  const shortCounts = names
    .map(shortNameOf)
    .reduce<Record<string, number>>(
      (counts, short) => ({ ...counts, [short]: (counts[short] ?? 0) + 1 }),
      {},
    );
  return Object.fromEntries(
    names.map((name) => [
      name,
      shortCounts[shortNameOf(name)] === 1 ? shortNameOf(name) : qualifiedNameOf(name),
    ]),
  );
};

const withRenamedRefs =
  (renames: Record<string, string>) =>
  (node: JsonObject): JsonObject =>
    typeof node.$ref === 'string' && node.$ref.startsWith(schemaRefPrefix)
      ? {
          ...node,
          $ref: `${schemaRefPrefix}${renames[node.$ref.slice(schemaRefPrefix.length)] ?? node.$ref.slice(schemaRefPrefix.length)}`,
        }
      : node;

const refTo = (name: string): JsonObject => ({
  $ref: `${schemaRefPrefix}${name}`,
});

const metaPrefix = 'io.k8s.apimachinery.pkg.apis.meta.v1.';

const jsonBody = (schema: JsonObject): JsonObject => ({
  content: { [jsonContent]: { schema } },
});

const okResponse = (schema: JsonObject): JsonObject => ({
  responses: { '200': { description: 'OK', ...jsonBody(schema) } },
});

const pathParameter = (name: string): JsonObject => ({
  in: 'path',
  name,
  required: true,
  schema: { type: 'string' },
});

const queryParameter = (name: string): JsonObject => ({
  in: 'query',
  name,
  schema: { type: 'string' },
});

type CrdVersion = {
  group: string;
  version: string;
  kind: string;
  plural: string;
  isNamespaced: boolean;
  schema: JsonObject;
};

const toCrdVersions = (manifest: Json): CrdVersion[] => {
  const spec = isObject(manifest) && isObject(manifest.spec) ? manifest.spec : {};
  const names = isObject(spec.names) ? spec.names : {};
  const versions = Array.isArray(spec.versions) ? spec.versions : [];
  return isObject(manifest) && manifest.kind === 'CustomResourceDefinition'
    ? versions.filter(isObject).map((version) => ({
        group: String(spec.group),
        version: String(version.name),
        kind: String(names.kind),
        plural: String(names.plural),
        isNamespaced: spec.scope === 'Namespaced',
        schema:
          isObject(version.schema) && isObject(version.schema.openAPIV3Schema)
            ? version.schema.openAPIV3Schema
            : { type: 'object' },
      }))
    : [];
};

const toCrdSlice = (crd: CrdVersion): Slice => {
  const qualifier = `${toPascal(crd.group)}${toPascal(crd.version)}`;
  const scoped = crd.isNamespaced ? `Namespaced${crd.kind}` : crd.kind;
  const root = `/apis/${crd.group}/${crd.version}`;
  const collection = crd.isNamespaced
    ? `${root}/namespaces/{namespace}/${crd.plural}`
    : `${root}/${crd.plural}`;
  const scopeParameters = crd.isNamespaced ? [pathParameter('namespace')] : [];
  const item = refTo(crd.kind);
  const list = refTo(`${crd.kind}List`);
  const listParameters = ['labelSelector', 'fieldSelector', 'limit', 'continue'].map(
    queryParameter,
  );
  const schemaProperties = isObject(crd.schema.properties) ? crd.schema.properties : {};
  return {
    paths: {
      ...(crd.isNamespaced
        ? {
            [`${root}/${crd.plural}`]: {
              get: {
                operationId: `list${qualifier}${crd.kind}ForAllNamespaces`,
                parameters: listParameters,
                ...okResponse(list),
              },
            },
          }
        : {}),
      [collection]: {
        parameters: scopeParameters,
        get: {
          operationId: `list${qualifier}${scoped}`,
          parameters: listParameters,
          ...okResponse(list),
        },
        post: {
          operationId: `create${qualifier}${scoped}`,
          requestBody: jsonBody(item),
          ...okResponse(item),
        },
      },
      [`${collection}/{name}`]: {
        parameters: [...scopeParameters, pathParameter('name')],
        get: { operationId: `read${qualifier}${scoped}`, ...okResponse(item) },
        put: {
          operationId: `replace${qualifier}${scoped}`,
          requestBody: jsonBody(item),
          ...okResponse(item),
        },
        patch: {
          operationId: `patch${qualifier}${scoped}`,
          requestBody: jsonBody({ type: 'object' }),
          ...okResponse(item),
        },
        delete: {
          operationId: `delete${qualifier}${scoped}`,
          ...okResponse(refTo(`${metaPrefix}Status`)),
        },
      },
    },
    schemas: {
      [crd.kind]: {
        ...crd.schema,
        properties: {
          apiVersion: { type: 'string' },
          kind: { type: 'string' },
          ...schemaProperties,
          metadata: refTo(`${metaPrefix}ObjectMeta`),
        },
      },
      [`${crd.kind}List`]: {
        type: 'object',
        required: ['items'],
        properties: {
          apiVersion: { type: 'string' },
          kind: { type: 'string' },
          metadata: refTo(`${metaPrefix}ListMeta`),
          items: { type: 'array', items: item },
        },
      },
    },
  };
};

const fromCrdManifest = async (relativePath: string): Promise<Slice[]> =>
  parseAllDocuments(await readFile(resolve(here, relativePath), 'utf8'))
    .map((document) => document.toJS() as Json)
    .flatMap(toCrdVersions)
    .map(toCrdSlice);

const merged = (slices: Slice[]): Slice => ({
  paths: Object.assign({}, ...slices.map((slice) => slice.paths)),
  schemas: Object.assign({}, ...slices.map((slice) => slice.schemas)),
});

const sortedByKey = <Value>(record: Record<string, Value>): Record<string, Value> =>
  Object.fromEntries(Object.entries(record).sort(([left], [right]) => left.localeCompare(right)));

const toSubset = ({ paths, schemas }: Slice): JsonObject => {
  const kept = [...reachableFrom(refsIn(paths), schemas)];
  const renamed = withRenamedRefs(toRenames(kept));
  const renames = toRenames(kept);
  return {
    openapi: '3.0.0',
    info: {
      title: 'Kubernetes API subset for the Stratum dashboard',
      version: kubernetesTag,
    },
    paths: sortedByKey(mapJson(paths, renamed) as JsonObject),
    components: {
      schemas: sortedByKey(
        Object.fromEntries(kept.map((name) => [renames[name], mapJson(schemas[name], renamed)])),
      ),
    },
  };
};

const upstreamSlices = await Promise.all(
  Object.entries(allowedGroupVersions).map(async ([groupVersion, allowed]) =>
    toSlice(groupVersion, allowed, await fromUpstream(groupVersion)),
  ),
);
const crdSlices = (await Promise.all(crdManifests.map(fromCrdManifest))).flat();
const subset = toSubset(merged([...upstreamSlices, ...crdSlices]));

await writeFile(resolve(here, 'k8s.subset.json'), `${JSON.stringify(subset, null, 2)}\n`);
