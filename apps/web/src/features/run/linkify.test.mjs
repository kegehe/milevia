import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transform } from "esbuild";

const source = await readFile(new URL("./linkify.ts", import.meta.url), "utf8");
const { code } = await transform(source, { loader: "ts", format: "esm", target: "es2022" });
const { linkifyText } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

test("splits plain text around a dev-server URL", () => {
  assert.deepEqual(linkifyText("Local: http://localhost:5173/ ready"), [
    { text: "Local: " },
    { text: "http://localhost:5173/", url: "http://localhost:5173/" },
    { text: " ready" },
  ]);
});

test("keeps the URL when it is the whole line", () => {
  assert.deepEqual(linkifyText("http://127.0.0.1:3000"), [
    { text: "http://127.0.0.1:3000", url: "http://127.0.0.1:3000" },
  ]);
});

test("strips trailing sentence punctuation from the link but keeps it visible", () => {
  assert.deepEqual(linkifyText("open http://x.com/a, now"), [
    { text: "open " },
    { text: "http://x.com/a", url: "http://x.com/a" },
    { text: "," },
    { text: " now" },
  ]);
});

test("balances closing brackets around a URL", () => {
  assert.deepEqual(linkifyText("(see http://x.com/a)"), [
    { text: "(see " },
    { text: "http://x.com/a", url: "http://x.com/a" },
    { text: ")" },
  ]);
  assert.deepEqual(linkifyText("http://en.wikipedia.org/wiki/X_(band)"), [
    { text: "http://en.wikipedia.org/wiki/X_(band)", url: "http://en.wikipedia.org/wiki/X_(band)" },
  ]);
});

test("does not match a URL glued to a word without a boundary", () => {
  assert.deepEqual(linkifyText("nothttps://x.com"), [{ text: "nothttps://x.com" }]);
  assert.deepEqual(linkifyText("a.bhttps://x.com"), [{ text: "a.bhttps://x.com" }]);
});

test("matches multiple URLs on one line", () => {
  assert.deepEqual(linkifyText("a http://a.com b http://b.com"), [
    { text: "a " },
    { text: "http://a.com", url: "http://a.com" },
    { text: " b " },
    { text: "http://b.com", url: "http://b.com" },
  ]);
});

test("keeps a bare scheme fragment as plain text", () => {
  assert.deepEqual(linkifyText("scheme https:// only"), [{ text: "scheme https:// only" }]);
});
