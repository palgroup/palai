import type { NextConfig } from "next";
import { resolve } from "node:path";

const nextConfig: NextConfig = {
  poweredByHeader: false,
  productionBrowserSourceMaps: true,
  turbopack: {
    root: resolve(process.cwd(), "../.."),
  },
  // Next 16 resolves the removed TypeScript 5 JavaScript API. The build
  // harness runs the pinned TypeScript 7 CLI before invoking Next instead.
  typescript: {
    ignoreBuildErrors: true,
  },
};

export default nextConfig;
