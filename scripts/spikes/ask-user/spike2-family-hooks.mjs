// Spike 2：CodeBuddy / Qoder 的 --settings hooks 是否与 Claude 同构？
//
// 分两段测，因为两段回答的是不同问题：
//   A. 命令注入 —— 让 hook 把 "echo ORIGINAL_VALUE" 改写成 "echo SPIKE_INJECTED_OK"。
//      只要这个生效，就证明 PreToolUse 触发 + updatedInput 被采纳，**与提问工具有没有
//      暴露无关**。这一段是判断"能不能一次覆盖两个 CLI"的关键。
//   B. 提问注入 —— 只有 init 工具表里出现 AskUserQuestion 时才跑。
//
// 结论口径：
//   A PASS → hook 通道可用；B 再 PASS 才等于"原生提问可用"，B INCONCLUSIVE 说明只是模型没拿到工具。
//   A FAIL → 这两个 CLI 的 hooks 与 Claude 不同构，原生通道直接放弃，改用 MCP 兜底。
import path from "node:path";
import { writeFileSync } from "node:fs";
import {
  artifactDir, runCli, parseJsonl, writeJson, hookPayloads, which,
  toolUseIdByName, toolResultText, initTools, finalResult,
} from "./lib/common.mjs";

const BASH_MARKER = "SPIKE_INJECTED_OK";
const ASK_MARKER = "SPIKE_ANSWER_BLUE";
const hookPath = path.resolve(import.meta.dirname, "lib/hook.mjs");
const hookCommand = '"' + process.execPath + '" "' + hookPath.replace(/\\/g, "/") + '"';

const TARGETS = [
  { id: "codebuddy", bin: "codebuddy", args: ["-p", "--output-format", "stream-json"] },
  { id: "qoder", bin: "qoder", args: ["-p", "-o", "stream-json"] },
];

const results = [];

for (const target of TARGETS) {
  if (!which(target.bin)) {
    results.push({ name: target.id, verdict: "INCONCLUSIVE", detail: "未安装，跳过" });
    continue;
  }

  const dir = artifactDir("spike2-" + target.id);
  const settingsPath = path.join(dir, "settings.json");
  writeJson(settingsPath, {
    hooks: {
      PreToolUse: [
        { matcher: ".*", hooks: [{ type: "command", command: hookCommand, timeout: 120 }] },
      ],
    },
  });

  // ── 段 A：命令改写 ──────────────────────────────────────────────
  const runA = await runCli(
    target.bin,
    [...target.args, "--settings", settingsPath],
    {
      input: "Run this exact shell command and then report its raw output verbatim: echo ORIGINAL_VALUE",
      env: {
        ...process.env,
        ASK_SPIKE_LOG: path.join(dir, "hooks-a"),
        ASK_SPIKE_MODE: "inject-bash",
        ASK_SPIKE_COMMAND: "echo " + BASH_MARKER,
      },
      timeoutMs: 180000,
    }
  );
  writeFileSync(path.join(dir, "phase-a.jsonl"), runA.stdout);
  writeFileSync(path.join(dir, "phase-a.stderr.txt"), runA.stderr);

  const eventsA = parseJsonl(runA.stdout);
  const toolsA = initTools(eventsA);
  const hooksA = hookPayloads(path.join(dir, "hooks-a"));
  const bashUseId = toolUseIdByName(eventsA, "Bash") || toolUseIdByName(eventsA, "Shell");
  const bashResult = bashUseId ? toolResultText(eventsA, bashUseId) : "";
  const bashInjected = (bashResult + "\n" + finalResult(eventsA)).includes(BASH_MARKER);

  let verdictA = "INCONCLUSIVE";
  let detailA = "";
  if (eventsA.length === 0) {
    detailA = "没有解析到任何事件；可能 --settings 被拒或该 CLI 不读 stdin。stderr=" +
      JSON.stringify(runA.stderr.slice(0, 200));
  } else if (hooksA.length === 0) {
    verdictA = "FAIL";
    detailA = "hook 一次都没触发（--settings 的 hooks 段未生效或不支持 PreToolUse）";
  } else if (!bashUseId) {
    detailA = "hook 有效，但这轮没调用命令工具，判不了 updatedInput：" +
      JSON.stringify(finalResult(eventsA).slice(0, 150));
  } else if (!bashInjected) {
    verdictA = "FAIL";
    detailA = "hook 触发但命令未被改写：updatedInput 不生效。tool_result=" +
      JSON.stringify(bashResult.slice(0, 150));
  } else {
    verdictA = "PASS";
    detailA = "命令被 hook 改写并执行，updatedInput 生效";
  }

  // ── 段 B：提问注入 ──────────────────────────────────────────────
  const hasAskTool = toolsA.some((name) => /askuserquestion/i.test(String(name)));
  let verdictB = "INCONCLUSIVE";
  let detailB = "init 工具表未暴露 AskUserQuestion，判不了";

  if (hasAskTool) {
    const runB = await runCli(
      target.bin,
      [...target.args, "--settings", settingsPath],
      {
        input: "Use the AskUserQuestion tool to ask me which color I prefer, options 'red' and 'blue'. " +
          "Do not ask in plain text. Then reply: COLOR=<answer>",
        env: {
          ...process.env,
          ASK_SPIKE_LOG: path.join(dir, "hooks-b"),
          ASK_SPIKE_MODE: "inject-questions",
          ASK_SPIKE_ANSWER: ASK_MARKER,
        },
        timeoutMs: 180000,
      }
    );
    writeFileSync(path.join(dir, "phase-b.jsonl"), runB.stdout);
    writeFileSync(path.join(dir, "phase-b.stderr.txt"), runB.stderr);

    const eventsB = parseJsonl(runB.stdout);
    const askUseId = toolUseIdByName(eventsB, "AskUserQuestion");
    const askResult = askUseId ? toolResultText(eventsB, askUseId) : "";
    const hooksB = hookPayloads(path.join(dir, "hooks-b"));
    if (!askUseId) {
      detailB = "工具有但模型没调用";
    } else if (hooksB.length === 0) {
      verdictB = "FAIL";
      detailB = "提问工具被调用但 hook 未触发";
    } else if (!(askResult + "\n" + finalResult(eventsB)).includes(ASK_MARKER)) {
      verdictB = "FAIL";
      detailB = "hook 触发但答案未生效。tool_result=" + JSON.stringify(askResult.slice(0, 150));
    } else {
      verdictB = "PASS";
      detailB = "答案注入生效";
    }
  }

  writeJson(path.join(dir, "verdict.json"), {
    cli: target.id,
    phaseA: { verdict: verdictA, detail: detailA, exitCode: runA.code, durationMs: runA.ms, hookCalls: hooksA.length },
    phaseB: { verdict: verdictB, detail: detailB },
    initTools: toolsA,
    hookPayloadFiles: hooksA.map((hook) => hook.file),
    artifactDir: dir,
  });

  results.push({ name: target.id + " · 命令改写", verdict: verdictA, detail: detailA });
  results.push({ name: target.id + " · 提问注入", verdict: verdictB, detail: detailB });
}

const width = results.reduce((max, row) => Math.max(max, row.name.length), 0);
console.log("\n==== Spike 2 · CodeBuddy / Qoder hooks 兼容性 ====\n");
for (const row of results) {
  console.log(row.name.padEnd(width) + "  " + String(row.verdict).padEnd(13) + "  " + row.detail);
}
const anyFail = results.some((row) => row.verdict === "FAIL");
const anyPass = results.some((row) => row.verdict === "PASS");
console.log("\n退出码 " + (anyFail ? 1 : anyPass ? 0 : 2));
process.exit(anyFail ? 1 : anyPass ? 0 : 2);
