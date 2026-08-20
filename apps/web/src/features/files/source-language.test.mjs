import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transform } from "esbuild";

const source = await readFile(new URL("./source-language.ts", import.meta.url), "utf8");
const { code } = await transform(source, { loader: "ts", format: "esm", target: "es2022" });
const { detectLanguage, getPreviewKind } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

test("detects source languages through one registry", () => {
  assert.equal(detectLanguage("schema.sql"), "sql");
  assert.equal(detectLanguage("Main.java"), "java");
  assert.equal(detectLanguage("worker.go"), "go");
  assert.equal(detectLanguage("config.yaml"), "yaml");
});

test("routes special previews before text content is loaded", () => {
  assert.equal(getPreviewKind("data.sqlite3", false, "", 100), "sqlite");
  assert.equal(getPreviewKind("photo.png", false, "image/png", 20 * 1024 * 1024), "image");
  assert.equal(getPreviewKind("settings.jsonc", true, "", 100), "json");
  assert.equal(getPreviewKind("large.py", true, "", 11 * 1024 * 1024), "large");
});
