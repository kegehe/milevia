// Spike 1：Claude Code 的 AskUserQuestion 能否用 PreToolUse hook 的 updatedInput 作答？
//
// 判定标准（必须三者同时成立才算 PASS）：
//   1. init 帧的 tools 里有 AskUserQuestion —— 否则模型根本没这个工具，环境不成立；
//   2. 模型真的调用了它；
//   3. hook 返回的 answers 出现在该 tool_use 的 tool_result 里，或出现在最终回复里。
//
// 三种结论的用法：
//   PASS  → 原生动提问通道可行，可以在 claude 上做（但要注意：靠模型/账号，按会话探测）
//   FAIL  → updatedInput 对提问工具不生效，原生通道**不存在**，只能走 MCP 兜底
//   INCONCLUSIVE → 这台机器/这个账号判不了，换有 Anthropic 直连的环境重跑
import path from "node:path";
import { writeFileSync } from "node:fs";
import {
  artifactDir, runCli, parseJsonl, writeJson, hookPayloads,
  toolUseIdByName, toolResultText, initTools, finalResult, report,
} from "./lib/common.mjs";

const ANSWER_MARKER = "SPIKE_ANSWER_BLUE";
const PROMPT = [
  "Use the AskUserQuestion tool to ask me a single question: which color do I prefer, with exactly two options 'red' and 'blue'.",
  "Do not ask in plain text; the tool is the only acceptable way.",
  "After the tool returns, reply with exactly one line: COLOR=<the answer you received>",
].join("\n");

const dir = artifactDir("spike1-claude-ask");
const hookPath = path.resolve(import.meta.dirname, "lib/hook.mjs");
const settingsPath = path.join(dir, "settings.json");

// hook 命令里 node 用绝对路径：claude 通过平台 shell 执行 hook，PATH 不一定与
// 当前进程一致；绝对路径能排除这个变量。
const hookCommand = '"' + process.execPath + '" "' + hookPath.replace(/\\/g, "/") + '"';
writeJson(settingsPath, {
  hooks: {
    PreToolUse: [
      { matcher: "AskUserQuestion", hooks: [{ type: "command", command: hookCommand, timeout: 120 }] },
    ],
  },
});

const run = await runCli(
  "claude",
  ["-p", "--output-format", "stream-json", "--verbose", "--settings", settingsPath],
  {
    input: PROMPT,
    env: {
      ...process.env,
      ASK_SPIKE_LOG: dir,
      ASK_SPIKE_MODE: "inject-questions",
      ASK_SPIKE_ANSWER: ANSWER_MARKER,
    },
    timeoutMs: 180000,
  }
);

writeFileSync(path.join(dir, "stdout.jsonl"), run.stdout);
writeFileSync(path.join(dir, "stderr.txt"), run.stderr);

const events = parseJsonl(run.stdout);
const tools = initTools(events);
const askToolUseId = toolUseIdByName(events, "AskUserQuestion");
const resultText = askToolUseId ? toolResultText(events, askToolUseId) : "";
const reply = finalResult(events);
const hooks = hookPayloads(dir);

const hasTool = tools.some((name) => /askuserquestion/i.test(String(name)));
const markerSeen = (resultText + "\n" + reply).includes(ANSWER_MARKER);

let verdict = "INCONCLUSIVE";
let detail = "";
if (!hasTool) {
  detail = "init 工具表里没有 AskUserQuestion（当前模型/账号未暴露），换 Anthropic 直连账号重跑";
} else if (!askToolUseId) {
  detail = "工具有但没有被调用，看 stdout.jsonl 里模型的说明；必要时把 prompt 写得更强制";
} else if (hooks.length === 0) {
  verdict = "FAIL";
  detail = "工具被调用了但 hook 没触发（--settings 里的 hooks 段没生效）";
} else if (!markerSeen) {
  verdict = "FAIL";
  detail = "hook 触发了但答案没生效：updatedInput.answers 被忽略。" +
    " tool_result=" + JSON.stringify(resultText.slice(0, 160));
} else {
  verdict = "PASS";
  detail = "hook 注入的答案出现在结果里，updatedInput 对提问工具生效";
}

writeJson(path.join(dir, "verdict.json"), {
  verdict,
  detail,
  initTools: tools,
  askToolUseId,
  hookCalls: hooks.length,
  toolResult: resultText.slice(0, 2000),
  finalReply: reply.slice(0, 2000),
  durationMs: run.ms,
  exitCode: run.code,
  artifactDir: dir,
});

console.log("原始证据：" + dir);
console.log("init 工具数：" + tools.length + "，含 AskUserQuestion：" + hasTool);
console.log("hook 调用次数：" + hooks.length);
if (resultText) console.log("tool_result：" + resultText.slice(0, 300));
if (reply) console.log("最终回复：" + reply.slice(0, 300));

process.exit(report("Spike 1 · claude AskUserQuestion + updatedInput", [
  { name: "claude-code", verdict, detail },
]));
