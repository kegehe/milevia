import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transform } from "esbuild";

const source = await readFile(new URL("./ansi.ts", import.meta.url), "utf8");
const { code } = await transform(source, { loader: "ts", format: "esm", target: "es2022" });
const { toAnsiSegments } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

test("ANSI SGR colors are rendered as safe segments", () => {
  assert.deepEqual(toAnsiSegments("\u001b[32mready\u001b[0m now"), [
    { text: "ready", className: "ansi-green" },
    { text: " now", className: undefined },
  ]);
});

test("unsupported terminal controls do not leak into the browser", () => {
  assert.deepEqual(toAnsiSegments("\u001b[2Kdone"), [{ text: "done", className: undefined }]);
});
