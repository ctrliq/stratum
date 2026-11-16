import reactConfig from '@ctrliq/quantic-eslint-config/react';
import strictConfig from '@ctrliq/quantic-eslint-config/strict';

export default [
  {
    ignores: [
      'dist',
      'src/routeTree.gen.ts',
      'src/api/*/*Components.ts',
      'src/api/*/*Schemas.ts',
      'src/api/*/*Utils.ts',
      'src/api/*/*Responses.ts',
      'src/api/*/*Parameters.ts',
      'src/api/*/*RequestBodies.ts',
    ],
  },
  ...reactConfig,
  ...strictConfig,
];
