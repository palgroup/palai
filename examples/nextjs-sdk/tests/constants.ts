// Shared between the Playwright config (which injects them into the servers) and the
// browser test (which scans for the sentinel). The API key is a distinctive sentinel so
// the browser-surface secret scan is meaningful: this exact string is the server-only
// credential, and it must appear in NO browser surface (request headers, source maps,
// static chunks). The Route Handler is its only holder.
export const API_KEY = "palai-sk-live-proof-DO-NOT-LEAK-7f3c9a1e2b8d4056";
export const NEXT_PORT = 3100;
export const UPSTREAM_PORT = 3101;
