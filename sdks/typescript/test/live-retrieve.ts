// live-retrieve is the @palai/sdk leg of the E16 T8 four-client parity journey. Given a response id in
// PALAI_LIVE_RESPONSE_ID and a live server (PALAI_BASE_URL + PALAI_API_KEY, the SDK's env defaults), it
// retrieves that SHARED response over the REAL SDK client and prints its NORMALIZED projection
// {"id","output_text","status"} on stdout — the exact shape the CLI + Go + Python legs emit. The journey
// canonical-bytes-diffs the four decodes. A 410 tombstone (a purged store:false response) prints a gone
// marker and exits 3, so the journey can assert the TYPED gone surface. Driven as:
//   node --experimental-strip-types sdks/typescript/test/live-retrieve.ts
import { Palai } from "../src/client.ts";
import { PalaiAPIError } from "../src/errors.ts";

async function main(): Promise<void> {
  const id = process.env.PALAI_LIVE_RESPONSE_ID;
  if (!id) {
    process.stderr.write("PALAI_LIVE_RESPONSE_ID is unset\n");
    process.exit(2);
  }
  const client = new Palai(); // reads PALAI_BASE_URL + PALAI_API_KEY
  try {
    const resp = await client.responses.retrieve(id);
    const items = (resp.output ?? []) as Array<Record<string, unknown>>;
    const text = items.map((it) => (typeof it.text === "string" ? it.text : "")).join("");
    process.stdout.write(JSON.stringify({ id: resp.id, output_text: text, status: resp.status }) + "\n");
  } catch (e) {
    if (e instanceof PalaiAPIError && e.status === 410) {
      process.stdout.write(JSON.stringify({ gone: true, status: 410 }) + "\n");
      process.exit(3);
    }
    process.stderr.write(String(e) + "\n");
    process.exit(1);
  }
}

await main();
