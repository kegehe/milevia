// Spike 3：HTTP 形态的 MCP 在各 CLI 上通不通？一次调用能挂多久？
//
// 这条测试的是"平台兜底通道"的可行性：如果走 HTTP MCP，Windows / WSL / SSH 三端
// 只需要一份配置（SSH 侧已有 curl 直连控制服务的先例），不用打包 linux 二进制。
//
// 每个 CLI 跑两档等待：短的只验连通，长的用来逼出 CLI 自己的工具调用超时。
// 长档挂掉时，delta 就是默认超时的下界——这正是"阻塞式提问能挂多久"的硬上限。
//
// 用法：
//   node spike3-http-mcp.mjs
//   node spike3-http-mcp.mjs --sleeps=5,90 --clis=claude,codex
import path from "node:path";
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import { artifactDir, runCli, writeJson, which } from "./lib/common.mjs";

function arg(name, fallback) {
  const hit = process.argv.find((value) => value.startsWith("--" + name + "="));
  return hit ? hit.slice(name.length + 3) : fallback;
}

const sleeps = String(arg("sleeps", "5,90")).split(",").map((v) => Number(v.trim())).filter((v) => v > 0);
const wanted = String(arg("clis", "claude,codebuddy,qoder,codex")).split(",").map((v) => v.trim());
// 单次 CLI 调用的上限。默认 180s 是为了容纳长档等待；排障时可以用
// --timeout-ms=120000 把"根本跑不完一轮的 CLI"快速判成 INCONCLUSIVE。
const runTimeoutMs = Number(arg("timeout-ms", "0")) || 0;
const dir = artifactDir("spike3-http-mcp");
const mcpLog = path.join(dir, "mcp-server.log");
const serverPath = path.resolve(import.meta.dirname, "lib/fake-mcp-server.mjs");

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
  server.stderr.on("data", (chunk) => { process.stderr.write(chunk); });
});
console.log("假 MCP server 端口：" + port);

const url = "http://127.0.0.1:" + port + "/mcp";
const configPath = path.join(dir, "mcp-config.json");
writeJson(configPath, {
  mcpServers: {
    spike: { type: "http", url, headers: { Authorization: "Bearer spike-token" } },
  },
});

const TOKEN = "spike-token";
// Windows 用 cmd.exe，单引号是字面量、能原样传给 TOML；POSIX shell 会吃掉单引号，
// 那里必须换成双引号。两处都是为了让 codex 收到合法的 TOML 字面量。
const q = process.platform === "win32" ? "'" : '"';

const CLIENTS = {
  claude: {
    bin: "claude",
    args: () => ["-p", "--output-format", "stream-json", "--verbose",
      "--mcp-config", configPath, "--strict-mcp-config",
      "--allowedTools", "mcp__spike__sleep"],
    env: () => ({}),
    prompt: (seconds) => "Call the MCP tool named sleep on the server spike with seconds=" + seconds +
      ". Then reply with exactly the text it returned.",
  },
  // codebuddy 把 MCP 调用包在 DeferExecuteTool 后面，非交互模式下那次调用自己
  // 要审批，--allowedTools 覆盖不到它。不放开权限模式时，现象是"传输通了但调用被拒"。
  codebuddy: {
    bin: "codebuddy",
    args: () => ["-p", "--output-format", "stream-json",
      "--permission-mode", "bypassPermissions",
      "--mcp-config", configPath, "--strict-mcp-config"],
    env: () => ({}),
    prompt: (seconds) => "Call the MCP tool named sleep on the server spike with seconds=" + seconds +
      ". Then reply with exactly the text it returned.",
  },
  // qoder 在 headless 下对 MCP 工具调用仍然走它自己的权限面：不带 bypass 时
  // 工具结果会是一句 "Error: Allow MCP tool ..."，看起来像连不上，其实是没放行。
  // 这里用 bypass_permissions 把"传输能不能通"和"策略放不放行"分开测。
  qoder: {
    bin: "qoder",
    args: () => ["-p", "-o", "stream-json", "--permission-mode", "bypass_permissions",
      "--mcp-config", configPath],
    env: () => ({}),
    prompt: (seconds) => "Call the MCP tool named sleep on the server spike with seconds=" + seconds +
      ". Then reply with exactly the text it returned.",
  },
  codex: {
    bin: "codex",
    args: () => ["exec",
      "-c", "mcp_servers.spike.url=" + q + url + q,
      "-c", "mcp_servers.spike.bearer_token_env_var=" + q + "SPIKE_MCP_TOKEN" + q,
      // tool_timeout_sec 是数字，不能加引号：传成字符串 codex 会直接报
      // "invalid type: string \"600\", expected f64" 并拒绝启动。
      "-c", "mcp_servers.spike.tool_timeout_sec=600",
      "--json", "--skip-git-repo-check", "-"],
    env: () => ({ SPIKE_MCP_TOKEN: TOKEN }),
    prompt: (seconds) => "Call the MCP tool named sleep on the server spike with seconds=" + seconds +
      ". Then reply with exactly the text it returned.",
  },
};

const rows = [];

for (const id of wanted) {
  const client = CLIENTS[id];
  if (!client) { rows.push({ name: id, verdict: "INCONCLUSIVE", detail: "未知 CLI" }); continue; }
  if (!which(client.bin)) { rows.push({ name: id, verdict: "INCONCLUSIVE", detail: "未安装，跳过" }); continue; }

  for (const seconds of sleeps) {
    const marker = "SPIKE_MCP_OK_" + seconds;
    const run = await runCli(client.bin, client.args(seconds), {
      input: client.prompt(seconds),
      env: { ...process.env, ...client.env() },
      cwd: dir,
      timeoutMs: runTimeoutMs || Math.max(180000, (seconds + 90) * 1000),
    });
    const slug = id + "-" + seconds + "s";
    writeFileSync(path.join(dir, slug + ".jsonl"), run.stdout);
    writeFileSync(path.join(dir, slug + ".stderr.txt"), run.stderr);

    const ok = run.stdout.includes(marker);
    const lowered = (run.stdout + "\n" + run.stderr).toLowerCase();
    // 先排掉 spike 自己的参数错误：否则 "invalid type ... expected f64" 这种
    // 配置失败会被 timeout 正则误判成"被超时掐断"，结论正好反了。
    const configError = /error loading config|invalid type|unexpected argument|unknown option/.test(lowered);
    const timedOut = !configError && /timed out|timeout|超时|deadline/.test(lowered);
    const permissionDenied = /allow mcp tool|permission denied|requires approval|not allowed/.test(lowered);

    let verdict = "FAIL";
    let detail = "";
    if (ok) {
      verdict = "PASS";
      detail = "调用成功，耗时 " + Math.round(run.ms / 1000) + "s";
    } else if (configError) {
      detail = "spike 参数错误（不是 CLI 的问题），看 " + slug + ".stderr.txt";
    } else if (permissionDenied) {
      detail = "传输已连通，但该 CLI 的策略层拦住了 MCP 工具调用，需要放开权限模式";
    } else if (timedOut) {
      detail = "疑似被工具超时掐断（耗时 " + Math.round(run.ms / 1000) + "s），看 " + slug + ".jsonl";
    } else if (run.killed || run.code === null) {
      // 被我们自己的上限掐掉：说明这个 CLI 连普通回合都跑不完，与 MCP 无关。
      // 判 INCONCLUSIVE 而不是 FAIL——别把环境问题记成"HTTP MCP 不可用"。
      verdict = "INCONCLUSIVE";
      detail = "CLI 自身没跑完一轮（被 spike 上限 " + Math.round(run.ms / 1000) + "s 终止），先修该 CLI 的可用性";
    } else {
      detail = "没拿到标记；可能没连上 MCP 或模型没调用工具，看 " + slug + ".jsonl";
    }
    rows.push({ name: id + " · " + seconds + "s", verdict, detail });

    writeJson(path.join(dir, "verdict-" + slug + ".json"), {
      cli: id, seconds, verdict, detail, exitCode: run.code,
      durationMs: run.ms, command: run.command,
    });
  }
}

server.kill();
writeJson(path.join(dir, "summary.json"), { port, sleeps, rows, artifactDir: dir });

const width = rows.reduce((max, row) => Math.max(max, row.name.length), 0);
console.log("\n==== Spike 3 · HTTP MCP 连通性与工具超时 ====\n");
for (const row of rows) {
  console.log(row.name.padEnd(width) + "  " + String(row.verdict).padEnd(13) + "  " + row.detail);
}
console.log("\n证据目录：" + dir);
const anyFail = rows.some((row) => row.verdict === "FAIL");
const anyPass = rows.some((row) => row.verdict === "PASS");
console.log("退出码 " + (anyFail ? 1 : anyPass ? 0 : 2));
process.exit(anyFail ? 1 : anyPass ? 0 : 2);
