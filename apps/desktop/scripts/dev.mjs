import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const desktopRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const isWindows = process.platform === "win32";
const packageManager = isWindows ? "pnpm.cmd" : "pnpm";
const tauriCli = resolve(desktopRoot, "node_modules/@tauri-apps/cli/tauri.js");

let activeChild = null;
let stopping = false;

function start(command, args) {
  const child = spawn(command, args, {
    cwd: desktopRoot,
    stdio: "inherit",
    shell: isWindows && command.endsWith(".cmd"),
    windowsHide: false,
  });
  activeChild = child;
  return new Promise((resolveExit, reject) => {
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (activeChild === child) activeChild = null;
      resolveExit({ code, signal });
    });
  });
}

function stopProcessTree(child) {
  if (!child?.pid) return;
  if (isWindows) {
    spawnSync("taskkill.exe", ["/PID", String(child.pid), "/T", "/F"], {
      stdio: "ignore",
      windowsHide: true,
    });
  } else if (child.exitCode === null) {
    child.kill("SIGTERM");
  }
}

function stopLifecycleShells() {
  // pnpm uses nested cmd.exe wrappers. Stop every matching ancestor, not just
  // the first one, otherwise an outer wrapper can still ask for confirmation.
  if (!isWindows) return;
  let pid = process.ppid;
  let lifecycleChain = false;
  const cmdAncestors = [];
  for (let depth = 0; depth < 8 && pid > 0; depth += 1) {
    const query = spawnSync("powershell.exe", [
      "-NoProfile", "-NonInteractive", "-Command",
      `(Get-CimInstance Win32_Process -Filter 'ProcessId = ${pid}' | Select-Object Name,CommandLine,ParentProcessId | ConvertTo-Json -Compress)`,
    ], { encoding: "utf8", windowsHide: true });
    let info;
    try { info = JSON.parse(query.stdout?.trim() ?? "{}"); } catch { info = {}; }
    const name = String(info.Name ?? "").toLowerCase();
    const commandLine = String(info.CommandLine ?? "").toLowerCase();
    const isDesktopShell = name === "cmd.exe"
      && (commandLine.includes("scripts/dev.mjs")
        || (commandLine.includes("pnpm") && commandLine.includes("desktop")));
    const isDesktopPnpm = name === "node.exe"
      && commandLine.includes("pnpm")
      && commandLine.includes("desktop")
      && commandLine.includes("dev");
    const parentPid = Number(info.ParentProcessId);
    lifecycleChain ||= isDesktopShell || isDesktopPnpm;
    if (name === "cmd.exe") cmdAncestors.push(pid);
    if (!Number.isInteger(parentPid) || parentPid === pid) break;
    pid = parentPid;
  }
  if (!lifecycleChain) return;
  for (const cmdPid of cmdAncestors) {
    spawnSync("taskkill.exe", ["/PID", String(cmdPid), "/T", "/F"], {
      stdio: "ignore",
      windowsHide: true,
    });
  }
}

function stopAll(exitCode) {
  if (stopping) return;
  stopping = true;
  stopProcessTree(activeChild);
  stopLifecycleShells();
  process.exitCode = exitCode;
}

process.once("SIGINT", () => stopAll(130));
process.once("SIGTERM", () => stopAll(143));

try {
  const prepare = await start(packageManager, ["run", "prepare-assets"]);
  if (stopping) process.exit(130);
  if (prepare.code !== 0 && !stopping) throw new Error(`prepare-assets exited with code ${prepare.code ?? 1}`);

  const build = await start(process.execPath, ["scripts/build-sidecar.mjs"]);
  if (stopping) process.exit(130);
  if (build.code !== 0 && !stopping) throw new Error(`build-sidecar exited with code ${build.code ?? 1}`);

  const tauri = await start(process.execPath, [tauriCli, "dev"]);
  if (!stopping && tauri.code !== 0) process.exitCode = tauri.code ?? 1;
} catch (error) {
  if (!stopping) {
    console.error(error instanceof Error ? error.message : error);
    process.exitCode = 1;
  }
}
