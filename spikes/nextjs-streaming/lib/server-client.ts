import "server-only";

type PalaiEnvironmentName =
  | "PALAI_SPIKE_API_KEY"
  | "PALAI_SPIKE_UPSTREAM_URL";

export interface OpenPalaiStreamOptions {
  lastEventId: string | null;
  signal: AbortSignal;
}

export function openPalaiStream({
  lastEventId,
  signal,
}: OpenPalaiStreamOptions): Promise<Response> {
  const apiKey = requiredPalaiEnvironment("PALAI_SPIKE_API_KEY");
  const upstreamURL = parseUpstreamURL(
    requiredPalaiEnvironment("PALAI_SPIKE_UPSTREAM_URL"),
  );
  const headers = new Headers({
    Accept: "text/event-stream",
    Authorization: `Bearer ${apiKey}`,
  });
  if (lastEventId !== null) {
    headers.set("Last-Event-ID", lastEventId);
  }
  return fetch(upstreamURL, {
    cache: "no-store",
    headers,
    method: "GET",
    signal,
  });
}

function requiredPalaiEnvironment(name: PalaiEnvironmentName): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`Missing required Palai environment variable: ${name}`);
  }
  return value;
}

function parseUpstreamURL(value: string): URL {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new Error("PALAI_SPIKE_UPSTREAM_URL must be an absolute URL");
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("PALAI_SPIKE_UPSTREAM_URL must use HTTP or HTTPS");
  }
  return url;
}
