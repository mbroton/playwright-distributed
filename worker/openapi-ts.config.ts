import { defineConfig } from '@hey-api/openapi-ts';

export default defineConfig({
  input: '../server/openapi.yaml',
  output: {
    path: 'src/api',
    module: {
      extension: '.js',
    },
  },
  plugins: [
    '@hey-api/typescript',
    {
      name: '@hey-api/sdk',
      client: '@hey-api/client-fetch',
    },
  ],
});
