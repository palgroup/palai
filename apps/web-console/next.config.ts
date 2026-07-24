import type { NextConfig } from "next";
import { resolve } from "node:path";

const nextConfig: NextConfig = {
  poweredByHeader: false,
  // Emit browser source maps so the public-API-only secret scan can prove the API key is absent
  // from them too, not just the minified chunks (examples/nextjs-sdk precedent).
  productionBrowserSourceMaps: true,
  // The workspace SDK ships raw TypeScript (its exports point at ./src/*.ts), so Next must
  // compile it rather than treat it as a prebuilt dependency.
  transpilePackages: ["@palai/sdk"],
  turbopack: {
    root: resolve(process.cwd(), "../.."),
  },
  // Next 16 resolves the removed TypeScript 5 JavaScript API; the repo pins TypeScript 7.
  // Typechecking is a separate CLI concern (`pnpm typecheck`), not the build.
  typescript: {
    ignoreBuildErrors: true,
  },
};

export default nextConfig;
