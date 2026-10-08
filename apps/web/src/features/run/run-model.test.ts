import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { createRunCommand, isRunnableCommand, runCommandLabel, runLogPresentation, runLogText, selectedRunCommand, withRunCommands, type RunConfig } from "./run-model.ts";

function baseConfig(overrides: Partial<RunConfig> = {}): RunConfig {
	return { workDir: "", command: "", envVars: {}, executionTarget: "auto", commands: [], selectedCommandId: "", ...overrides };
}

test("resolves the selected command and falls back to the first one", () => {
	const commands = [
		{ id: "a", name: "开发环境", command: "npm run dev" },
		{ id: "b", name: "预发", command: "npm run dev:staging" },
	];
	assert.equal(selectedRunCommand(baseConfig({ commands, selectedCommandId: "b" }))?.id, "b");
	// 选中项被删/未选时回落到第一条，界面不会出现"没有选中项"的空态。
	assert.equal(selectedRunCommand(baseConfig({ commands, selectedCommandId: "gone" }))?.id, "a");
	assert.equal(selectedRunCommand(baseConfig({ commands, selectedCommandId: "" }))?.id, "a");
	assert.equal(selectedRunCommand(baseConfig()), undefined);
});

test("never treats a command without command text as the selected one", () => {
	// 空行只是「添加一条」留下的壳，后端保存时会丢弃它。若界面把它当成选中项，启动时就会
	// 发出一个后端不认识的命令 ID（"选中的启动命令已不存在"）。
	const commands = [
		{ id: "a", name: "开发环境", command: "npm run dev" },
		{ id: "blank", name: "", command: "" },
	];
	assert.equal(isRunnableCommand(commands[0]), true);
	assert.equal(isRunnableCommand(commands[1]), false);
	assert.equal(isRunnableCommand({ id: "b", name: "预发", command: "   " }), false);

	assert.equal(selectedRunCommand(baseConfig({ commands, selectedCommandId: "blank" }))?.id, "a");
	// 全部是空行时没有任何选中项，与后端"命令列表为空 → 请先配置启动命令"的判定一致。
	assert.equal(selectedRunCommand(baseConfig({ commands: [commands[1]], selectedCommandId: "blank" })), undefined);
	// 反之，列表里只要还有一条真命令，选中项就不会停在空行上。
	assert.equal(selectedRunCommand(baseConfig({ commands: [...commands, { id: "c", name: "", command: "go run ." }], selectedCommandId: "blank" }))?.id, "a");
});

test("keeps the selected command valid and the command mirror in sync on every edit", () => {
	const commands = [
		{ id: "a", name: "开发环境", command: "npm run dev" },
		{ id: "b", name: "预发", command: "npm run dev:staging" },
	];

	// 删除选中项后自动落到剩下的第一条，command 镜像跟着变。
	const removed = withRunCommands(baseConfig({ commands, selectedCommandId: "b" }), commands.filter((item) => item.id !== "a"));
	assert.equal(removed.selectedCommandId, "b");
	assert.equal(removed.command, "npm run dev:staging");
	const removedSelected = withRunCommands(baseConfig({ commands, selectedCommandId: "b" }), commands.filter((item) => item.id !== "b"), "");
	assert.equal(removedSelected.selectedCommandId, "a");
	assert.equal(removedSelected.command, "npm run dev");

	// 追加一条空命令不会顶掉当前选中项，镜像也不动。
	const appended = withRunCommands(baseConfig({ commands, selectedCommandId: "a", command: "npm run dev" }), [...commands, createRunCommand()]);
	assert.equal(appended.selectedCommandId, "a");
	assert.equal(appended.command, "npm run dev");

	// 清空所有命令时不能留下悬空的选中项或旧命令。
	const cleared = withRunCommands(baseConfig({ commands, selectedCommandId: "b", command: "npm run dev:staging" }), []);
	assert.deepEqual(cleared.commands, []);
	assert.equal(cleared.selectedCommandId, "");
	assert.equal(cleared.command, "");

	// 原配置不被就地修改（React 依赖引用变化决定重渲染）。
	const original = baseConfig({ commands, selectedCommandId: "a" });
	withRunCommands(original, [], "");
	assert.equal(original.selectedCommandId, "a");
});

test("labels a command by name, then by its text, then by position", () => {
	assert.equal(runCommandLabel({ id: "a", name: " 开发环境 ", command: "npm run dev" }, 0), "开发环境");
	assert.equal(runCommandLabel({ id: "a", name: "", command: " npm run dev " }, 0), "npm run dev");
	assert.equal(runCommandLabel({ id: "a", name: "", command: "" }, 2), "命令 3");
	// 每条新建的命令 ID 都不同：列表 key 与选中判定都靠它。
	assert.notEqual(createRunCommand().id, createRunCommand().id);
});

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
