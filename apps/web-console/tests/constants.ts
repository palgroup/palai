// Shared between the Playwright config (which injects them into the servers) and the tests (which scan
// for the sentinel + assert the relay origin). The API key is a distinctive sentinel so the browser-surface
// secret scan is meaningful: this exact string is the server-only credential, and it must appear in NO
// browser surface (request headers, URLs, bodies, source maps, static chunks). The relay is its only holder.
export const API_KEY = "palai-sk-console-proof-DO-NOT-LEAK-1a2b3c4d5e6f7a8b";
export const NEXT_PORT = 3200;
export const UPSTREAM_PORT = 3201;
export const UPSTREAM = `http://127.0.0.1:${UPSTREAM_PORT}`;
