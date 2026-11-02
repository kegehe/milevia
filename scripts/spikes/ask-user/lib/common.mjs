// 三个提问交互 spike 的共享工具。零依赖，只用 Node 内置模块。
//
// 为什么不引第三方包：spike 的结论要能被别人直接复跑，任何依赖都会变成
// "先装依赖再复现"的额外步骤。
import { spawn, spawnSync } from "node:child_process";
import { mkdirSync, writeFileSync, readFileSync, existsSync, readdirSync } from "node:fs";
import path from "node:path";

// artifactDir 给每次运行一个独立目录：原始事件流、stderr、hook 收到的 payload
// 全部落盘。spike 的价值一半在结论，另一半在于别人能翻原始证据复核。
export function artifactDir(name) {
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const dir = path.resolve(process.cwd(), ".tmp", "spikes", "ask-user", name + "-" + stamp);
  mkdirSync(dir, { recursive: true });
  return dir;
}

// runCli 用 shell 拉起 CLI。
//
// Windows 上 claude/codebuddy/qoder 都是 npm 的 .cmd shim，Node 在 shell:false 下
// 无法直接执行；统一走 shell 后，参数**必须**保证不含空格与双引号，否则 cmd.exe
// 会把它们拆开。所有调用点都把配置写文件、把 TOML 值写成单引号字面量，就是为了
// 满足这一条。
export function runCli(command, args, opts = {}) {
  const { input = "", env = process.env, cwd = process.cwd(), timeoutMs = 300000 } = opts;
  const line = [command, ...args].join(" ");
  const startedAt = Date.now();
  return new Promise((resolve) => {
    const child = spawn(line, { shell: true, cwd, env, windowsHide: true });
    let stdout = "";
    let stderr = "";
    let killed = false;
    // Windows 上 child.kill() 只杀掉 shell（cmd.exe），真正的 CLI 是它的子进程，
    // 会带着 stdout 管道继续活着 —— 表现为"超时了但 close 永远不触发"，spike 直接挂死。
    // 必须整棵树杀，并且在 close 不回来时强制结算，保证脚本一定有界。
    const finish = (code, signal) => {
      clearTimeout(timer);
      resolve({ code, signal, stdout, stderr, ms: Date.now() - startedAt, command: line, killed });
    };
    const timer = setTimeout(() => {
      killed = true;
      try {
        if (process.platform === "win32") {
          spawnSync("taskkill", ["/pid", String(child.pid), "/T", "/F"]);
        } else {
          process.kill(-child.pid, "SIGKILL");
        }
      } catch {}
      // 给 taskkill 一点时间收尾；不回来就自己结算，绝不让调用方无限等。
      setTimeout(() => finish(null, "SIGKILL"), 3000);
    }, timeoutMs);
    child.stdout.on("data", (chunk) => { stdout += chunk.toString(); });
    child.stderr.on("data", (chunk) => { stderr += chunk.toString(); });
    child.on("error", (err) => {
      stderr += String(err);
      finish(-1, null);
    });
    child.on("close", (code, signal) => finish(code, signal));
    child.stdin.end(input);
  });
}

// parseJsonl 容忍坏行：CLI 的 stdout 里常混进进度行或半个 JSON。
export function parseJsonl(text) {
  const events = [];
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line.startsWith("{")) continue;
    try { events.push(JSON.parse(line)); } catch {}
  }
  return events;
}

export function which(bin) {
  const probe = process.platform === "win32"
    ? spawnSync("where", [bin], { encoding: "utf8" })
    : spawnSync("sh", ["-c", "command -v " + bin], { encoding: "utf8" });
  if (probe.status !== 0) return null;
  const first = String(probe.stdout || "").split(/\r?\n/)[0].trim();
  return first || null;
}

export function writeJson(file, value) {
  writeFileSync(file, JSON.stringify(value, null, 2) + "\n");
}

export function readJson(file) {
  return JSON.parse(readFileSync(file, "utf8"));
}

// hookPayloads 收集 hook.mjs 落盘的原始 payload，供人工核对 schema 差异。
export function hookPayloads(dir) {
  if (!existsSync(dir)) return [];
  return readdirSync(dir)
    .filter((name) => name.startsWith("hook-") && name.endsWith(".json"))
    .sort()
    .map((name) => {
      try { return { file: name, body: JSON.parse(readFileSync(path.join(dir, name), "utf8")) }; }
      catch { return { file: name, body: null }; }
    });
}

// claudeFamilyToolResult 从 claude 系 stream-json 里取某个 tool_use_id 的结果文本。
export function toolResultText(events, toolUseId) {
  for (const event of events) {
    if (event.type !== "user") continue;
    const content = event.message && event.message.content;
    if (!Array.isArray(content)) continue;
    for (const part of content) {
      if (part && part.type === "tool_result" && part.tool_use_id === toolUseId) {
        return stringifyContent(part.content);
      }
    }
  }
  return "";
}

function stringifyContent(content) {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content.map((part) => (part && typeof part.text === "string" ? part.text : JSON.stringify(part))).join("\n");
  }
  if (content == null) return "";
  return JSON.stringify(content);
}

// toolUseIdByName 找第一条指定工具名的 tool_use 调用 id。
export function toolUseIdByName(events, name) {
  for (const event of events) {
    if (event.type !== "assistant") continue;
    const content = event.message && event.message.content;
    if (!Array.isArray(content)) continue;
    for (const part of content) {
      if (part && part.type === "tool_use" && part.name === name) return part.id;
    }
  }
  return "";
}

export function initTools(events) {
  for (const event of events) {
    if (event.type === "system" && event.subtype === "init" && Array.isArray(event.tools)) return event.tools;
  }
  return [];
}

export function finalResult(events) {
  let text = "";
  for (const event of events) {
    if (event.type === "result" && typeof event.result === "string") text = event.result;
  }
  return text;
}

// report 打印结论表并按严重度决定退出码：
//   0 = 有结论且通过
//   1 = 有结论且失败（这就是我们要的答案：此路不通）
//   2 = 无结论（当前环境无法判定，换环境重跑）
export function report(title, rows) {
  const width = rows.reduce((max, row) => Math.max(max, row.name.length), 0);
  console.log("\n==== " + title + " ====\n");
  for (const row of rows) {
    console.log(
      row.name.padEnd(width) + "  " + String(row.verdict).padEnd(13) + "  " + row.detail
    );
  }
  const failed = rows.some((row) => row.verdict === "FAIL");
  const passed = rows.some((row) => row.verdict === "PASS");
  const code = failed ? 1 : passed ? 0 : 2;
  console.log("\n退出码 " + code + "（0=通过 1=失败 2=无法判定）");
  return code;
}
