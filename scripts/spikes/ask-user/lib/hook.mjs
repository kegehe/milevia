// 被 CLI 当作 hook 命令执行的进程。
//
// 做三件事：把 stdin 原文落盘（schema 差异全靠它）、把结果写成 hook 输出 JSON、
// 永远 exit 0（hook 非零退出会让 CLI 把整次调用判成失败，掩盖我们真正想测的东西）。
//
// 行为由环境变量控制：
//   ASK_SPIKE_LOG      payload 落盘目录
//   ASK_SPIKE_MODE     inject-questions | inject-bash | observe
//   ASK_SPIKE_ANSWER   注入问题答案用的标记文本
//   ASK_SPIKE_COMMAND  注入 Bash 命令时替换成的命令
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import path from "node:path";

const dir = process.env.ASK_SPIKE_LOG || "";
const mode = process.env.ASK_SPIKE_MODE || "observe";
const answer = process.env.ASK_SPIKE_ANSWER || "SPIKE_ANSWER";
const injectedCommand = process.env.ASK_SPIKE_COMMAND || "echo SPIKE_INJECTED_OK";

const chunks = [];
process.stdin.on("data", (chunk) => chunks.push(chunk));
process.stdin.on("end", () => {
  const raw = Buffer.concat(chunks).toString("utf8");
  let payload = null;
  try { payload = JSON.parse(raw); } catch {}

  if (dir) {
    try {
      mkdirSync(dir, { recursive: true });
      const stamp = new Date().toISOString().replace(/[:.]/g, "-");
      const name = "hook-" + stamp + "-" + Math.random().toString(36).slice(2, 7) + ".json";
      writeFileSync(path.join(dir, name), raw || "<empty>");
    } catch {}
  }

  if (!payload) {
    process.stdout.write(JSON.stringify({}));
    return;
  }

  const toolName = String(payload.tool_name || "");
  const toolInput = payload.tool_input && typeof payload.tool_input === "object" ? payload.tool_input : null;

  if (mode === "observe") {
    process.stdout.write(JSON.stringify({}));
    return;
  }

  if (mode === "inject-questions") {
    // 只对提问工具动作；其它工具一律放行，避免 spike 被无关的审批卡住。
    if (!/askuserquestion/i.test(toolName) || !toolInput) {
      process.stdout.write(JSON.stringify(allowOutput("spike: pass through")));
      return;
    }
    const questions = Array.isArray(toolInput.questions) ? toolInput.questions : [];
    const answers = {};
    for (const question of questions) {
      const key = String((question && question.question) || "");
      if (key) answers[key] = answer;
    }
    process.stdout.write(JSON.stringify(allowOutput("spike: answered by operator", {
      ...toolInput,
      answers,
    })));
    return;
  }

  if (mode === "inject-bash") {
    if (!/bash|shell/i.test(toolName) || !toolInput) {
      process.stdout.write(JSON.stringify(allowOutput("spike: pass through")));
      return;
    }
    process.stdout.write(JSON.stringify(allowOutput("spike: rewritten command", {
      ...toolInput,
      command: injectedCommand,
    })));
    return;
  }

  process.stdout.write(JSON.stringify({}));
});

function allowOutput(reason, updatedInput) {
  const output = {
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "allow",
      permissionDecisionReason: reason,
    },
  };
  if (updatedInput) output.hookSpecificOutput.updatedInput = updatedInput;
  return output;
}
