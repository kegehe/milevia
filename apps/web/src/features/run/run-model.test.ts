import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { runLogPresentation, runLogText } from "./run-model.ts";

test("uses semantic labels for standard-error logs", () => {
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Running BeforeDevCommand (`npm run dev`)" }), { label: "错误输出", tone: "" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Warn Waiting for your frontend dev server" }), { label: "警告", tone: "is-warning" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Error failed to start" }), { label: "错误", tone: "is-error" });
});

test("shows the original error log text verbatim", () => {
  assert.equal(runLogText({ stream: "stderr", text: "Error failed to start" }), "Error failed to start");
  assert.equal(runLogText({ stream: "stderr", text: "npm ERR! code ERESOLVE" }), "npm ERR! code ERESOLVE");
  assert.equal(runLogText({ stream: "stderr", text: "ModuleNotFoundError: Cannot find module x" }), "ModuleNotFoundError: Cannot find module x");
  assert.equal(runLogText({ stream: "stderr", text: "sh: vite: not found" }), "sh: vite: not found");
  assert.equal(runLogText({ stream: "stderr", text: "permission denied" }), "permission denied");
  assert.equal(runLogText({ stream: "stderr", text: "exit code 1" }), "exit code 1");
  assert.equal(runLogText({ stream: "stderr", text: "go: module example.com/foo: malformed module path" }), "go: module example.com/foo: malformed module path");
  assert.equal(runLogText({ stream: "stderr", text: "错误：配置无效" }), "错误：配置无效");
});

test("reserves room for semantic labels and lets their colors override stderr", () => {
	const styles = readFileSync(new URL("../../run.css", import.meta.url), "utf8");

	assert.match(styles, /grid-template-columns:\s*70px\s+52px\s+minmax\(0,\s*1fr\)/);
	assert.ok(styles.indexOf(".run-log-line.stderr .run-log-stream") < styles.indexOf(".run-log-line.is-warning .run-log-stream"));
	assert.match(styles, /\.run-log-line\.is-error \.run-log-stream\s*\{/);
});
