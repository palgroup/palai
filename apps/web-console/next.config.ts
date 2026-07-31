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
  // WITHOUT THIS, `next dev` SERVES A CONSOLE THAT NEVER HYDRATES ON 127.0.0.1 — and it does so silently.
  //
  // Next's dev server refuses cross-site requests to `/_next/*` against an allow-list it builds as
  // `['**.localhost', 'localhost', ...allowedDevOrigins]` plus whatever `--hostname` was given
  // (`server/lib/router-utils/block-cross-site-dev.ts`, Next 16.2.10). `127.0.0.1` is a DIFFERENT string
  // and is in none of those, so the HMR websocket upgrade — the one `/_next` request a browser makes with
  // an `Origin` header — is blocked. In Turbopack dev the client waits on that socket, so the page loads,
  // every chunk 200s, React's own DevTools banner prints, the flight payload is consumed, and then nothing
  // hydrates: no `__reactFiber$` on any element, `preventDefault` never runs, and the login form does a
  // native GET (`/login?password=…`). There is no hydration error, because hydration never started.
  //
  // MEASURED on 2026-07-31, one `next dev -p 3230` process, same build, ONLY the hostname in the URL
  // differing — which is also what rules out `turbopack.root`, `transpilePackages`, a stale `.next` and the
  // pnpm layout, since all four are identical across the two rows:
  //   http://127.0.0.1:3230/login → defaultPrevented=false, __reactFiber$ absent   (ws upgrade: blocked)
  //   http://localhost:3230/login → defaultPrevented=true,  __reactFiber$ present  (ws upgrade: 101)
  // Reproduced at the socket: the same handshake gets `101 Switching Protocols` with no `Origin` header or
  // with `Origin: http://localhost:3230`, and is refused with `Origin: http://127.0.0.1:3230`.
  //
  // 127.0.0.1 is not an incidental address here. It is what playwright.config.ts's baseURL uses, what
  // docs/operations/console.md's curl recipes use, and one of the two origins lib/session.ts names as
  // potentially-trustworthy so the `Secure` session cookie survives plain HTTP. This option is dev-only:
  // it relaxes nothing in `next start`, and it is not the console's own Origin check (that is lib/session.ts,
  // which is unchanged and still compares Origin against Host on every mutation).
  allowedDevOrigins: ["127.0.0.1"],
  // Typechecking is a separate CLI concern (`pnpm typecheck`), not the build.
  //
  // AND THIS FLAG IS CURRENTLY A NO-OP, which is worth writing down rather than leaving as a guess. The
  // comment here used to say Next "resolves the removed TypeScript 5 JavaScript API" — implying the build
  // would fail without the flag. MEASURED on 2026-07-31 by removing it, deleting `.next` and rebuilding: the
  // build SUCCEEDS, and Next prints `Detected @typescript/native-preview as TypeScript compiler. Some Next.js
  // TypeScript features (like type checking during build) require the standard typescript package.` So the
  // build does not typecheck either way; the flag only turns `Running TypeScript … 1135ms` into `TypeScript
  // config validation … 76ms`. There is no error backlog behind it either: `pnpm exec tsc --noEmit` is exit 0
  // with 0 errors on this commit (tsc 7.0.2). It is kept because it states the intent and costs nothing;
  // removing it is a separate decision, not a cleanup.
  typescript: {
    ignoreBuildErrors: true,
  },
};

export default nextConfig;
