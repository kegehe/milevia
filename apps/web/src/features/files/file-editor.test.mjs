import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./FileEditor.tsx", import.meta.url), "utf8");

test("editor reuses the shared source viewer", () => {
  assert.match(source, /import \{ CodeFileView \} from "\.\/CodeFileView"/);
  assert.match(source, /<CodeFileView content=\{content\} filename=\{stat\.name\}/);
});
