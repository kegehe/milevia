import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./SqliteViewer.tsx", import.meta.url), "utf8");

test("invalid SQLite files return to the binary download view", () => {
  assert.match(source, /code === "sqlite_not_database"/);
  assert.match(source, /onNotDatabase\(\)/);
});

test("table changes invalidate an older page request", () => {
  assert.match(source, /const rowsRequestVersion = useRef\(0\)/);
  assert.match(source, /const requestVersion = \+\+rowsRequestVersion\.current/);
  assert.match(source, /requestVersion === rowsRequestVersion\.current/);
});
