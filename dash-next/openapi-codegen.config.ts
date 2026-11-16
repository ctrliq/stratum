import { defineConfig } from '@openapi-codegen/cli';
import { generateReactQueryComponents, generateSchemaTypes } from '@openapi-codegen/typescript';

type GeneratorContext = Parameters<typeof generateSchemaTypes>[0];

const toClient = (filenamePrefix: string) => async (context: GeneratorContext) => {
  const { schemasFiles } = await generateSchemaTypes(context, {
    filenamePrefix,
  });
  await generateReactQueryComponents(context, {
    filenamePrefix,
    schemasFiles,
  });
};

export default defineConfig({
  k8s: {
    from: { source: 'file', relativePath: './openapi/k8s/k8s.subset.json' },
    outputDir: './src/api/k8s',
    to: toClient('k8s'),
  },
  dashboard: {
    from: { source: 'file', relativePath: './openapi/dashboard.openapi.yaml' },
    outputDir: './src/api/dashboard',
    to: toClient('dashboard'),
  },
  fleet: {
    from: { source: 'file', relativePath: './openapi/fleet.openapi.yaml' },
    outputDir: './src/api/fleet',
    to: toClient('fleet'),
  },
});
