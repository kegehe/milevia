// 真机验收脚本：对着本机跑起来的 control-server 走一遍「常用命令目录」接口（docs/37）。
//
// 与 verify-model-endpoints.mjs 的区别：那条链路的真机部分是"接口形状"，命令目录的真机
// 部分是**真的拉起一次 Claude CLI 探针**——这正是本特性最需要验证的地方（它必须零 token、
// 不需要凭据、只读第一行 init 就收工）。脚本会如实打印拿到的命令条数与来源。
//
// 用法：node scripts/verify-command-catalog.mjs
// 前置：先用临时 data-dir 起一个控制服务（见文件末尾注释）。
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const base = "http://127.0.0.1:18099";
const headers = { "X-Milevia-Session": "test-token-123", "Content-Type": "application/json" };

async function call(method, path, body) {
  const response = await fetch(base + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await response.text();
  let parsed = null;
  try { parsed = text ? JSON.parse(text) : null; } catch { parsed = text; }
  return { status: response.status, body: parsed };
}

const failures = [];
function check(label, condition, detail) {
  console.log(`${condition ? "OK  " : "FAIL"} ${label}${condition ? "" : " :: " + detail}`);
  if (!condition) failures.push(label);
}

// 造一个带项目级自定义命令的目录：目录必须按 cwd 生效，这条命令只有在探针用项目路径作
// cwd 时才会出现——它同时验证了"探针 cwd 正确"与"自定义命令进目录"两件事。
const projectPath = await mkdtemp(join(tmpdir(), "milevia-cmdcheck-"));
const commandsDir = join(projectPath, ".claude", "commands");
await mkdir(commandsDir, { recursive: true });
await writeFile(join(commandsDir, "probe-marker.md"), "---\ndescription: 验收标记命令\nargument-hint: \"[路径]\"\n---\n统计 TODO。\n");

const created = await call("POST", "/api/projects", { path: projectPath, name: "cmdcheck" });
check("注册项目", created.status === 200 || created.status === 201, JSON.stringify(created));
const projectId = created.body?.id ?? created.body?.project?.id;
check("拿到 projectId", Boolean(projectId), JSON.stringify(created.body));

if (projectId) {
  // 1) 徽标路径：probe=0 不得拉起 CLI，只给静态候选，且**不声称权威**（不权威才不会误报失效）。
  const cheap = await call("GET", `/api/projects/${projectId}/commands?agentId=claude-code&probe=0`);
  check("probe=0 返回 200", cheap.status === 200, JSON.stringify(cheap));
  check("probe=0 不声称权威", cheap.body?.authoritative === false, JSON.stringify(cheap.body?.source));
  check("probe=0 仍有候选命令", (cheap.body?.commands?.length ?? 0) > 0, JSON.stringify(cheap.body?.commands?.length));

  // 2) 探针路径：这一次真的拉起 CLI，拿到权威目录。
  const started = Date.now();
  const probed = await call("GET", `/api/projects/${projectId}/commands?agentId=claude-code&refresh=1`);
  const elapsed = Date.now() - started;
  check("refresh=1 返回 200", probed.status === 200, JSON.stringify(probed));
  check("refresh=1 声称权威", probed.body?.authoritative === true, JSON.stringify(probed.body?.note));
  check("带出 CLI 版本号", Boolean(probed.body?.claudeCodeVersion), JSON.stringify(probed.body?.claudeCodeVersion));
  const names = (probed.body?.commands ?? []).map((command) => command.name);
  console.log(`     来源=${probed.body?.source} 版本=${probed.body?.claudeCodeVersion} 命令数=${names.length} 探针耗时=${elapsed}ms`);
  console.log(`     前 8 条：${names.slice(0, 8).join(", ")}`);
  // 三条硬断言：目录必须来自 CLI（条数不会是个位数）、必须包含内置命令、必须包含项目自定义命令。
  check("目录条数是 CLI 量级（>20）", names.length > 20, `只有 ${names.length} 条`);
  check("含内置命令 compact", names.includes("compact"), names.join(","));
  check("含项目自定义命令 probe-marker", names.includes("probe-marker"), names.join(","));
  const marker = (probed.body?.commands ?? []).find((command) => command.name === "probe-marker");
  check("自定义命令带 frontmatter 描述", marker?.description === "验收标记命令", JSON.stringify(marker));
  check("自定义命令归到 project 分组", marker?.group === "project", JSON.stringify(marker?.group));
  const compact = (probed.body?.commands ?? []).find((command) => command.name === "compact");
  check("内置命令带中文名", Boolean(compact?.label), JSON.stringify(compact));
  if (names.includes("doctor")) {
    check("terminal_slash_commands 标记为仅终端", (probed.body.commands.find((c) => c.name === "doctor"))?.terminalOnly === true, "doctor 未标记");
  }

  // 3) 缓存：再读一次不该再拉起探针（耗时明显更短且来源不变）。
  const cached = await call("GET", `/api/projects/${projectId}/commands?agentId=claude-code`);
  check("第二次读命中原探针结果", cached.body?.source === probed.body?.source && cached.body?.commands?.length === names.length, JSON.stringify(cached.body?.source));

  // 4) Codex：不提供斜杠命令目录，但保留自定义 shell 命令入口。
  const codex = await call("GET", `/api/projects/${projectId}/commands?agentId=codex`);
  check("codex 无命令目录", (codex.body?.commands?.length ?? -1) === 0, JSON.stringify(codex.body?.commands?.length));
  check("codex 保留自定义入口", codex.body?.customAllowed === true, JSON.stringify(codex.body));
  check("codex 说明原因", Boolean(codex.body?.note), JSON.stringify(codex.body?.note));
}

// 清理尽力而为：Windows 上控制服务可能仍握着该目录（工作区锁），删不掉不该让验收脚本
// 看起来像失败——真正失败的是上面的断言。
try {
  await rm(projectPath, { recursive: true, force: true });
} catch (cause) {
  console.log(`     （临时目录未删除：${cause.code ?? cause.message}）`);
}
console.log(failures.length === 0 ? "\n全部通过" : `\n失败 ${failures.length} 项: ${failures.join(" | ")}`);
process.exit(failures.length === 0 ? 0 : 1);

// 复现方式（不影响正式库）：
//   cd apps/control-server && go build -o /tmp/milevia-ctl.exe ./cmd/control-server
//   /tmp/milevia-ctl.exe -mode desktop-api --session-token test-token-123 \
//     --allowed-origin http://localhost:5173 -addr 127.0.0.1:18099 -data-dir <临时目录> \
//     --approval-hook <仓库>/scripts/claude-approval-hook.sh
//   node scripts/verify-command-catalog.mjs
