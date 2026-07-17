// The API key path is server-only. Importing this module marks its importer as
// server-only: if the module is ever evaluated in a browser bundle, it throws instead of
// silently shipping the secret to the client. Bundlers that honor the package's "browser"
// export condition never reach a credential module (they resolve ./index.browser, which
// carries no key); this runtime check is the framework-agnostic backstop for bundlers and
// runtimes that do not. It is a no-op in Node and other server runtimes, where there is no
// window/document, so the SDK stays usable and unit-testable off any single framework.
//
// This is deliberately NOT the `server-only` npm package: that package throws on import in
// plain Node (its default export throws outside the React `react-server` condition), which
// would make the SDK Next.js-only and untestable in a Node test runner. A browser-shaped
// global check gives the same protection without that coupling.

const inBrowser =
  typeof (globalThis as { window?: unknown }).window !== "undefined" ||
  typeof (globalThis as { document?: unknown }).document !== "undefined";

if (inBrowser) {
  throw new Error(
    "@palai/sdk: a Palai client configured with an API key must never be bundled for the " +
      "browser — that would ship your secret to every visitor. Import it only from server " +
      "code (route handlers, server actions, server components), or use the browser-safe " +
      "'@palai/sdk/browser' entrypoint, which carries no credential.",
  );
}

export {};
