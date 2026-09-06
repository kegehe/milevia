import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

const desktopRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const debugDesktopBinary = resolve(desktopRoot, "src-tauri/target/debug/milevia-desktop.exe");
const agentEnvPath = resolve(desktopRoot, "../agent/.env.windows");
const isWindows = process.platform === "win32";
const packageManager = isWindows ? "pnpm.cmd" : "pnpm";
const tauriCli = resolve(desktopRoot, "node_modules/@tauri-apps/cli/tauri.js");

let activeChild = null;
let agentChild = null;
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

function agentAlreadyRunning() {
  if (!isWindows) return false;
  const result = spawnSync("powershell.exe", [
    "-NoProfile", "-NonInteractive", "-Command",
    "@(Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -match 'milevia-agent' }).Count -gt 0",
  ], { encoding: "utf8", windowsHide: true });
  return result.status === 0 && result.stdout.trim().toLowerCase() === "true";
}

function releaseDevPort() {
  const port = 1420;
  if (isWindows) {
    const result = spawnSync("powershell.exe", [
      "-NoProfile", "-NonInteractive", "-Command",
      `$items = @(Get-NetTCPConnection -State Listen -LocalPort ${port} -ErrorAction SilentlyContinue); ` +
      `$items | ForEach-Object { ` +
      `$p = Get-CimInstance Win32_Process -Filter \"ProcessId = $($_.OwningProcess)\"; ` +
      `[pscustomobject]@{Pid=$_.OwningProcess; CommandLine=$p.CommandLine} ` +
      `} | ConvertTo-Json -Compress`,
    ], { encoding: "utf8", windowsHide: true });
    let listeners;
    try { listeners = JSON.parse(result.stdout?.trim() || "[]"); } catch { listeners = []; }
    if (!Array.isArray(listeners)) listeners = [listeners];
    for (const listener of listeners) {
      const pid = Number(listener?.Pid);
      const commandLine = String(listener?.CommandLine ?? "").toLowerCase();
      const isMileviaVite = commandLine.includes("vite")
        && commandLine.includes("1420")
        && (commandLine.includes("milevia") || commandLine.includes("vite.config"));
      if (Number.isInteger(pid) && pid > 0 && isMileviaVite) {
        spawnSync("taskkill.exe", ["/PID", String(pid), "/T", "/F"], {
          stdio: "ignore",
          windowsHide: true,
        });
        const desktopProcesses = spawnSync("powershell.exe", [
          "-NoProfile", "-NonInteractive", "-Command",
          "@(Get-CimInstance Win32_Process -Filter \"Name = 'milevia-desktop.exe'\" | Select-Object ProcessId,ExecutablePath) | ConvertTo-Json -Compress",
        ], { encoding: "utf8", windowsHide: true });
        let desktopEntries;
        try { desktopEntries = JSON.parse(desktopProcesses.stdout?.trim() || "[]"); } catch { desktopEntries = []; }
        if (!Array.isArray(desktopEntries)) desktopEntries = [desktopEntries];
        const expectedBinary = debugDesktopBinary.replaceAll("\\", "/").toLowerCase();
        for (const entry of desktopEntries) {
          const desktopPid = Number(entry?.ProcessId);
          const executablePath = String(entry?.ExecutablePath ?? "").replaceAll("\\", "/").toLowerCase();
          if (Number.isInteger(desktopPid) && desktopPid > 0 && executablePath === expectedBinary) {
            spawnSync("taskkill.exe", ["/PID", String(desktopPid), "/T", "/F"], {
              stdio: "ignore",
              windowsHide: true,
            });
          }
        }
      }
    }
    return;
  }
  const result = spawnSync("sh", ["-lc", `lsof -tiTCP:${port} -sTCP:LISTEN 2>/dev/null`], { encoding: "utf8" });
  for (const value of (result.stdout ?? "").split(/\s+/)) {
    const pid = Number(value);
    if (!Number.isInteger(pid) || pid <= 0) continue;
    const command = spawnSync("ps", ["-p", String(pid), "-o", "command="], { encoding: "utf8" }).stdout ?? "";
    if (command.includes("vite") && command.includes("1420") && (command.includes("milevia") || command.includes("vite.config"))) {
      spawnSync("kill", ["-TERM", String(pid)], { stdio: "ignore" });
    }
  }
}

function stopAll(exitCode) {
  if (stopping) return;
  stopping = true;
  stopProcessTree(activeChild);
  stopProcessTree(agentChild);
  stopLifecycleShells();
  process.exitCode = exitCode;
}

process.once("SIGINT", () => stopAll(130));
process.once("SIGTERM", () => stopAll(143));

try {
  // Reclaim a stale Vite listener left by an interrupted Milevia desktop run.
  // The command-line guard prevents terminating an unrelated project that may
  // happen to use the same development port.
  releaseDevPort();
  // Reuse the local Agent configuration for the desktop sidecar during
  // development. The control server needs the cloud relay variables too;
  // previously only start-agent.ps1 loaded them, leaving pairing disabled in
  // the desktop app.
  if (existsSync(agentEnvPath)) {
    for (const line of readFileSync(agentEnvPath, "utf8").split(/\r?\n/)) {
      const match = line.trim().match(/^([^#=][^=]*)=(.*)$/);
      if (!match) continue;
      const [, key, value] = match;
      if (!process.env[key]) process.env[key] = value.trim();
    }
    process.env.AUTO_REMOTE_CLOUD_URL ||= process.env.MILEVIA_CLOUD_URL || "";
    process.env.AUTO_REMOTE_CLOUD_TOKEN ||= process.env.MILEVIA_CLOUD_AGENT_TOKEN || "";
    process.env.AUTO_REMOTE_INSTANCE_ID ||= process.env.MILEVIA_INSTANCE_ID || "";
  }
  // Keep the desktop and Cloud Control connected during development. The
  // relay is a separate process because it owns the outbound WSS connection
  // and forwards commands to the local control server.
  const agentScript = resolve(desktopRoot, "../agent/start-agent.ps1");
  if (isWindows && existsSync(agentScript) && process.env.MILEVIA_CLOUD_AGENT_TOKEN && !agentAlreadyRunning()) {
    agentChild = spawn("powershell.exe", ["-NoProfile", "-ExecutionPolicy", "Bypass", "-File", agentScript], {
      cwd: resolve(desktopRoot, "../agent"),
      stdio: "ignore",
      windowsHide: true,
    });
    agentChild.once("exit", () => { agentChild = null; });
  }
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
