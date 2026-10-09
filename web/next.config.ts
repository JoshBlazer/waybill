import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Self-contained server for the Compose image (deploy/compose.yaml).
  output: "standalone",
  cacheComponents: true,
  partialPrefetching: true,
  turbopack: {
    rules: {
      "*.css": {
        loaders: ["@tailwindcss/turbopack"],
        as: "*.css",
      },
    },
  },
};

export default nextConfig;
