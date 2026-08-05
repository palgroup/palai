// The packaging gate: pack @palai/sdk the way a release does, then INSTALL the tarball into an empty
// directory and import it. Every other test in this suite imports `../src/*.ts` directly, which is the
// right thing for testing behaviour and is exactly why none of them can see this class of failure — the
// package could export paths that are not in the tarball, and 64 green tests would say nothing.
//
// That failure was real here rather than hypothetical: before this file, `exports` pointed at
// `./src/index.ts` while `files` shipped only `dist`, so a consumer installing the package would have
// resolved to a file the tarball does not contain.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { after, test } from "node:test";
import assert from "node:assert/strict";

const sdkRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const scratch = mkdtempSync(join(tmpdir(), "palai-sdk-pack-"));
after(() => rmSync(scratch, { recursive: true, force: true }));

// packOnce builds the tarball a single time for every case below. `pnpm pack` runs the package's own
// prepack, so this exercises the real build rather than a hand-rolled tsc invocation.
let tarballPath: string | undefined;
function packOnce(): string {
	if (tarballPath) return tarballPath;
	execFileSync("pnpm", ["pack", "--pack-destination", scratch], { cwd: sdkRoot, stdio: "pipe" });
	const listing = execFileSync("ls", ["-1", scratch], { encoding: "utf8" }).trim().split("\n");
	const found = listing.find((f) => f.endsWith(".tgz"));
	assert.ok(found, `pnpm pack produced no tarball in ${scratch}: ${listing.join(", ")}`);
	tarballPath = join(scratch, found);
	return tarballPath;
}

function tarballEntries(): string[] {
	return execFileSync("tar", ["-tzf", packOnce()], { encoding: "utf8" })
		.trim()
		.split("\n")
		.map((p) => p.replace(/^package\//, ""));
}

test("the packed package ships compiled JavaScript and declarations, never TypeScript source", () => {
	const entries = tarballEntries();
	// `.d.ts` is a declaration, not source. A raw `.ts` in the tarball means the build did not run or
	// `files` is shipping src, and the consumer's runtime would be asked to parse types.
	const rawSource = entries.filter((p) => p.endsWith(".ts") && !p.endsWith(".d.ts"));
	assert.deepEqual(rawSource, [], `the tarball carries TypeScript source: ${rawSource.join(", ")}`);
	assert.ok(entries.includes("dist/index.js"), "no dist/index.js in the tarball");
	assert.ok(entries.includes("dist/index.d.ts"), "no dist/index.d.ts in the tarball");
	assert.ok(entries.includes("dist/index.admin.js"), "no dist/index.admin.js in the tarball");
	assert.ok(entries.includes("dist/index.browser.js"), "no dist/index.browser.js in the tarball");
});

test("every path the package exports is present in the tarball", () => {
	const manifest = JSON.parse(readFileSync(join(sdkRoot, "package.json"), "utf8")) as {
		exports: Record<string, unknown>;
		main?: string;
		types?: string;
	};
	const entries = new Set(tarballEntries());

	// Walk the export map rather than asserting three hard-coded names: a new subpath added later would
	// otherwise be unguarded, which is how an export map drifts away from what is actually shipped.
	const referenced: string[] = [];
	const walk = (node: unknown): void => {
		if (typeof node === "string") {
			referenced.push(node);
			return;
		}
		if (node && typeof node === "object") {
			for (const v of Object.values(node)) walk(v);
		}
	};
	walk(manifest.exports);
	if (manifest.main) referenced.push(manifest.main);
	if (manifest.types) referenced.push(manifest.types);
	assert.ok(referenced.length > 0, "the package declares no exports at all");

	for (const raw of referenced) {
		const rel = raw.replace(/^\.\//, "");
		assert.ok(entries.has(rel), `package.json points at ${raw}, which the tarball does not contain`);
	}
});

test("a clean consumer installs the tarball and calls the client", () => {
	const consumer = mkdtempSync(join(tmpdir(), "palai-sdk-consumer-"));
	writeFileSync(join(consumer, "package.json"), JSON.stringify({ name: "consumer-smoke", private: true, type: "module" }));

	// --offline proves the package needs nothing from a registry. The SDK declares no runtime
	// dependencies, so an install that reaches the network would mean one crept in unnoticed.
	execFileSync("npm", ["install", "--silent", "--offline", "--no-audit", "--no-fund", packOnce()], {
		cwd: consumer,
		stdio: "pipe",
	});

	writeFileSync(
		join(consumer, "use.mjs"),
		[
			`import { Palai } from "@palai/sdk";`,
			`const c = new Palai({ baseURL: "http://127.0.0.1:1/v1", apiKey: "sk-test" });`,
			`if (typeof c.responses?.create !== "function") { throw new Error("responses.create missing"); }`,
			`console.log("consumer-ok");`,
		].join("\n"),
	);
	const out = execFileSync("node", ["use.mjs"], { cwd: consumer, encoding: "utf8" });
	assert.match(out, /consumer-ok/, `the installed package did not construct a usable client: ${out}`);

	rmSync(consumer, { recursive: true, force: true });
});
