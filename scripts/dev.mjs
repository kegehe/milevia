import { createServer } from "node:net";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const rootDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");

if (process.platform !== "win32") {
  const child = spawn("bash", ["dev.sh"], { cwd: rootDir, stdio: "inherit" });
  child.once("exit", (code) => { process.exitCode = code ?? 1; });
} else {
  await startWindowsDev();
}

async function startWindowsDev() {
  const controlHost = process.env.AUTO_CONTROL_HOST ?? "127.0.0.1";
  const controlPort = parsePort("AUTO_CONTROL_PORT", 8080);
  const webHost = process.env.AUTO_WEB_HOST ?? "127.0.0.1";
  const webPort = parsePort("AUTO_WEB_PORT", 5173);
  const clearPorts = process.env.AUTO_CLEAR_PORTS === "1";
  const timeoutSeconds = parsePort("AUTO_STARTUP_TIMEOUT", 60);
  if (controlPort === webPort) throw new Error("AUTO_CONTROL_PORT and AUTO_WEB_PORT must be different.");

  for (const [host, port] of [[controlHost, controlPort], [webHost, webPort]]) {
    if (await portAvailable(host, port)) continue;
    if (!clearPorts) throw new Error(`Port ${port} is already in use. Set AUTO_CLEAR_PORTS=1 only to stop its listener.`);
    clearPort(port);
    if (!(await portAvailable(host, port))) throw new Error(`Port ${port} is still in use after cleanup.`);
  }

  const controlURL = process.env.AUTO_CONTROL_URL ?? `http://${controlHost}:${controlPort}`;
  const control = spawn("go.exe", ["run", "./cmd/control-server"], {
    cwd: resolve(rootDir, "apps/control-server"),
    env: { ...process.env, AUTO_HTTP_ADDR: `${controlHost}:${controlPort}`, AUTO_CONTROL_URL: controlURL },
    stdio: "inherit",
    windowsHide: false,
  });
  let web;
  const cleanup = () => {
    stopProcessTree(web);
    stopProcessTree(control);
  };
  process.once("SIGINT", () => { cleanup(); process.exit(130); });
  process.once("SIGTERM", () => { cleanup(); process.exit(143); });

  try {
    await waitForHealth(`${controlURL}/api/health`, timeoutSeconds, control);
    web = spawn("pnpm.cmd", ["dev", "--host", webHost, "--port", String(webPort), "--strictPort"], {
      cwd: resolve(rootDir, "apps/web"),
      env: { ...process.env, VITE_CONTROL_URL: process.env.VITE_CONTROL_URL ?? controlURL },
      stdio: "inherit",
      windowsHide: false,
      shell: true,
    });
    console.log(`Development server ready at http://${webHost}:${webPort}/`);
    const [name, code] = await Promise.race([
      waitForExit(control).then((exitCode) => ["control server", exitCode]),
      waitForExit(web).then((exitCode) => ["web server", exitCode]),
    ]);
    throw new Error(`${name} exited with code ${code ?? 1}.`);
  } finally {
    cleanup();
  }
}

function parsePort(name, fallback) {
  const value = Number(process.env[name] ?? fallback);
  if (!Number.isInteger(value) || value < 1 || value > 65535) throw new Error(`${name} must be a valid TCP port.`);
  return value;
}

function portAvailable(host, port) {
  return new Promise((resolveAvailable) => {
    const server = createServer();
    server.once("error", () => resolveAvailable(false));
    server.listen(port, host, () => server.close(() => resolveAvailable(true)));
  });
}

function clearPort(port) {
  const command = `Get-NetTCPConnection -State Listen -LocalPort ${port} -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique | ForEach-Object { Stop-Process -Id $_ -Force }`;
  execFileSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-Command", command], { stdio: "inherit" });
}

async function waitForHealth(url, timeoutSeconds, child) {
  const deadline = Date.now() + timeoutSeconds * 1000;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) throw new Error("Control server exited before becoming ready.");
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(1000) });
      if (response.ok) return;
    } catch {
      // The server is still starting.
    }
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 100));
  }
  throw new Error(`Control server did not become ready within ${timeoutSeconds} seconds.`);
}

function waitForExit(child) {
  return new Promise((resolveExit) => child.once("exit", resolveExit));
}

function stopProcessTree(child) {
  if (!child?.pid || child.exitCode !== null) return;
  spawnSync("taskkill.exe", ["/PID", String(child.pid), "/T", "/F"], { stdio: "ignore", windowsHide: true });
}
