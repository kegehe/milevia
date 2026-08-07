import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { runLogPresentation, runLogText } from "./run-model.ts";

test("uses semantic labels for standard-error logs", () => {
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Info Waiting for your frontend dev server" }), { label: "输出", tone: "is-info" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Warn Waiting for your frontend dev server" }), { label: "警告", tone: "is-warning" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Error failed to start" }), { label: "错误", tone: "is-error" });
});

test("treats build-tool success/progress lines on stderr as plain output, not errors", () => {
	// cargo/编译器把成功、进度与 info 信息也写到 stderr，这些无害行不能标成“错误输出”。
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Running DevCommand (`cargo run --no-default-features --color always --`)" }), { label: "输出", tone: "is-info" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Running BeforeDevCommand (`npm run dev`)" }), { label: "输出", tone: "is-info" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Finished `dev` profile [unoptimized + debuginfo] target(s) in 0.63s" }), { label: "输出", tone: "is-info" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Info Watching D:\\projects\\Programs\\Levitaire\\src-tauri for changes..." }), { label: "输出", tone: "is-info" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Compiling levitaire v0.1.0 (D:\\projects\\Programs\\Levitaire\\src-tauri\\src)" }), { label: "输出", tone: "is-info" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "Running `target\\debug\\levitaire.exe`" }), { label: "输出", tone: "is-info" });
	// 真正的错误与死代码警告仍保留各自语义。
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "error[E0433]: failed to resolve" }), { label: "错误", tone: "is-error" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "warning: struct `QuickInputSnippet` is never constructed" }), { label: "警告", tone: "is-warning" });
	assert.deepEqual(runLogPresentation({ stream: "stderr", text: "warning: `levitaire` (bin \"levitaire\") generated 2 warnings" }), { label: "警告", tone: "is-warning" });
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
	assert.ok(styles.indexOf(".run-log-line.stderr .run-log-stream") < styles.indexOf(".run-log-line.is-info .run-log-stream"));
	assert.match(styles, /\.run-log-line\.is-error \.run-log-stream\s*\{/);
});
