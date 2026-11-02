// Spike 4：能不能在"正常权限模式"下把 MCP 的 ask_user 放行？
//
// 为什么这条决定方案：MCP 兜底通道要求模型能真正调用 mcp__<server>__<tool>。
// 如果只在 bypass 一切权限时才能调，那这个通道在生产里等于不可用 —— 平台不可能
// 为了提问把整个会话的权限放开。
//
// 每一行都对应一个**生产里真实会出现的模式**，不是随手编的组合：
//   codebuddy  生产 workspace_write/full_control → --dangerously-skip-permissions
//   codebuddy  （无 flag）                       → 对照组，复现"被拒"的现象
//   codebuddy  默认模式 + permissions.allow       → 只读/受限模式下的候选放行方式
//   qoder      生产 workspace_write              → --permission-mode accept_edits
//   qoder      生产 read_only                    → --permission-mode default
//   qoder      accept_edits + --allowed-tools    → 受限模式下的候选放行方式
//
// 用法：
//   node spike4-permission.mjs                    # 两个 CLI 都跑
//   node spike4-permission.mjs --clis=codebuddy
import path from "node:path";
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import { artifactDir, runCli, writeJson, which } from "./lib/common.mjs";

function arg(name, fallback) {
  const hit = process.argv.find((value) => value.startsWith("--" + name + "="));
  return hit ? hit.slice(name.length + 3) : fallback;
}

const wanted = String(arg("clis", "codebuddy,qoder")).split(",").map((v) => v.trim());
const dir = artifactDir("spike4-permission");
const mcpLog = path.join(dir, "mcp-server.log");
const serverPath = path.resolve(import.meta.dirname, "lib/fake-mcp-server.mjs");
const configPath = path.join(dir, "mcp-config.json");

const server = spawn(process.execPath, [serverPath], {
  env: { ...process.env, SPIKE_MCP_LOG: mcpLog },
  stdio: ["ignore", "pipe", "pipe"],
});
const port = await new Promise((resolve, reject) => {
  const timer = setTimeout(() => reject(new Error("fake MCP server did not start")), 15000);
  let buffer = "";
  server.stdout.on("data", (chunk) => {
    buffer += chunk.toString();
    const match = buffer.match(/READY (\d+)/);
    if (match) { clearTimeout(timer); resolve(Number(match[1])); }
  });
});

const url = "http://127.0.0.1:" + port + "/mcp";
writeJson(configPath, {
  mcpServers: { spike: { type: "http", url, headers: { Authorization: "Bearer spike-token" } } },
});

const allowMcpSettings = path.join(dir, "allow-mcp-settings.json");
writeJson(allowMcpSettings, { permissions: { allow: ["mcp__spike__sleep"] } });

const allowDeferSettings = path.join(dir, "allow-defer-settings.json");
writeJson(allowDeferSettings, { permissions: { allow: ["DeferExecuteTool", "mcp__spike__sleep"] } });

const PROMPT = "Call the MCP tool named sleep on the server spike with seconds=1. " +
  "Then reply with exactly the text it returned. Do not use any other tool.";

// 每个 CLI 各自的参数装配器。mcp-config 固定带上，变量只有权限相关的那几个参数。
const CLIENTS = {
  codebuddy: {
    bin: "codebuddy",
    rows: [
      { id: "生产·skip-permissions", args: ["--dangerously-skip-permissions"] },
      { id: "对照·无权限参数", args: [] },
      { id: "受限·permissions.allow", args: ["--settings", allowMcpSettings] },
      { id: "受限·allow+Defer", args: ["--settings", allowDeferSettings] },
    ],
  },
  qoder: {
    bin: "qoder",
    rows: [
      { id: "生产·accept_edits", args: ["--permission-mode", "accept_edits"] },
      { id: "生产·默认(只读)", args: ["--permission-mode", "default"] },
      { id: "受限·allowed-tools", args: ["--permission-mode", "accept_edits", "--allowed-tools", "mcp__spike__sleep"] },
    ],
  },
};

const results = [];

for (const id of wanted) {
  const client = CLIENTS[id];
  if (!client) { results.push({ name: id, verdict: "INCONCLUSIVE", detail: "未知 CLI" }); continue; }
  if (!which(client.bin)) { results.push({ name: id, verdict: "INCONCLUSIVE", detail: "未安装" }); continue; }

  for (const row of client.rows) {
    const slug = id + "-" + row.id.replace(/[^a-zA-Z0-9]+/g, "-");
    const args = id === "qoder"
      ? ["-p", "-o", "stream-json", ...row.args, "--mcp-config", configPath]
      : ["-p", "--output-format", "stream-json", ...row.args, "--mcp-config", configPath, "--strict-mcp-config"];

    const run = await runCli(client.bin, args, {
      input: PROMPT,
      env: { ...process.env },
      cwd: dir,
      timeoutMs: 150000,
    });
    writeFileSync(path.join(dir, slug + ".jsonl"), run.stdout);
    writeFileSync(path.join(dir, slug + ".stderr.txt"), run.stderr);

    const ok = run.stdout.includes("SPIKE_MCP_OK_1");
    const lowered = (run.stdout + "\n" + run.stderr).toLowerCase();
    const denied = /allow mcp tool|requires approval|not allowed|权限|permission prompts are not available/.test(lowered);

    let verdict = "FAIL";
    let detail = "";
    if (ok) {
      verdict = "PASS";
      detail = "MCP 工具调用被放行（" + Math.round(run.ms / 1000) + "s）";
    } else if (run.killed) {
      verdict = "INCONCLUSIVE";
      detail = "跑不完一轮（被上限终止）";
    } else if (denied) {
      detail = "被权限层拦下：" + JSON.stringify(extractDenial(run.stdout).slice(0, 120));
    } else {
      detail = "没拿到标记，看 " + slug + ".jsonl";
    }

    results.push({ name: id + " · " + row.id, verdict, detail });
    writeJson(path.join(dir, "verdict-" + slug + ".json"), {
      cli: id, mode: row.id, verdict, detail, command: run.command, durationMs: run.ms,
    });
  }
}

server.kill();
writeJson(path.join(dir, "summary.json"), { port, results, artifactDir: dir });

const width = results.reduce((max, row) => Math.max(max, row.name.length), 0);
console.log("\n==== Spike 4 · MCP 工具在正常权限模式下能否放行 ====\n");
for (const row of results) {
  console.log(row.name.padEnd(width) + "  " + String(row.verdict).padEnd(13) + "  " + row.detail);
}
console.log("\n证据目录：" + dir);
const anyFail = results.some((row) => row.verdict === "FAIL");
const anyPass = results.some((row) => row.verdict === "PASS");
console.log("退出码 " + (anyFail ? 1 : anyPass ? 0 : 2));
process.exit(anyFail ? 1 : anyPass ? 0 : 2);

// 只认真正的拒绝文案。别用宽泛的 /permission/ —— 它会先命中证据文件名
// "permission-<时间戳>.jsonl"，结论栏里就变成一句毫无信息量的文件名。
function extractDenial(text) {
  const match = text.match(/Allow MCP tool[^"\\]{0,80}|permission prompts are not available[^"\\]{0,40}|需要[^"\\]{0,60}审批|requires approval[^"\\]{0,40}/);
  return match ? match[0] : "";
}
