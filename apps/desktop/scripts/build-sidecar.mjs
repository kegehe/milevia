import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { basename, dirname, extname, resolve } from "node:path";

const desktopRoot = resolve(import.meta.dirname, "..");
const controlRoot = resolve(desktopRoot, "../control-server");
const binaries = resolve(desktopRoot, "binaries");
const cacheFile = resolve(binaries, ".build-cache.json");
const controlOutput = resolve(binaries, "milevia-control.exe");
const approvalOutput = resolve(binaries, "milevia-approval.exe");
const terminalBridgeOutput = resolve(binaries, "milevia-terminal-bridge.exe");
const agentRoot = resolve(desktopRoot, "../agent");
const agentOutput = resolve(binaries, "milevia-agent.exe");
const agentConfigOutput = resolve(binaries, "milevia-agent.env");

if (process.platform !== "win32") {
  throw new Error("Windows desktop sidecar must be built on Windows so the CGO SQLite toolchain is reproducible.");
}

const force = process.argv.includes("--force");

/** Recursively collect files whose extension is in the given set under a
 *  directory, returning absolute paths. */
function collectFiles(root, extensions) {
  const files = [];
  function walk(dir) {
    const entries = readdirSync(dir, { withFileTypes: true });
    for (const entry of entries) {
      const full = resolve(dir, entry.name);
      if (entry.isDirectory()) { walk(full); }
      else if (extensions.has(extname(entry.name))) { files.push(full); }
    }
  }
  walk(root);
  return files;
}

/** Probe the CGO toolchain so environment changes invalidate the build cache.
 *  go-sqlite3 requires CGO; if CGO is off or no C compiler resolves, `go build`
 *  silently produces a stub binary that crashes at runtime. Folding these into
 *  the fingerprint prevents a previously-built stub from being reused as
 *  "up to date" just because the .go sources didn't change. */
function cgoFingerprint() {
  const cgoEnabled = process.env.CGO_ENABLED ?? "(unset)";
  let ccResolved = process.env.CC ?? "(unset)";
  let gccVersion = "(missing)";
  try {
    // `go env CC` honors CGO_ENABLED and reports the compiler go build will use.
    ccResolved = execFileSync("go", ["env", "CC"], { encoding: "utf-8" }).trim() || "(unset)";
    if (ccResolved && ccResolved !== "(unset)") {
      gccVersion = execFileSync(ccResolved, ["--version"], { encoding: "utf-8", stdio: ["ignore", "pipe", "ignore"] }).trim().split("\n")[0];
    }
  } catch { /* CC unresolvable — leave gccVersion as "(missing)" */ }
  return `CGO_ENABLED=${cgoEnabled}|CC=${ccResolved}|gcc=${gccVersion}`;
}

/** Compute a fingerprint of tracked source/config files:
 *  path → mtime mapping, hashed with SHA-256, plus the CGO toolchain state. */
function computeFingerprint(paths) {
  const hash = createHash("sha256");
  const sorted = paths.slice().sort();
  for (const p of sorted) {
    try { hash.update(`${p}@${statSync(p).mtimeMs}`); }
    catch { /* deleted file — fingerprint will differ next time */ }
  }
  hash.update(`|env=${cgoFingerprint()}`);
  return hash.digest("hex");
}

/** Return the cached fingerprint from a previous build, or null. */
function readCache() {
  try {
    if (!existsSync(cacheFile)) return null;
    return JSON.parse(readFileSync(cacheFile, "utf-8"));
  } catch { return null; }
}

/** Persist the fingerprint cache. */
function writeCache(fingerprint) {
  mkdirSync(dirname(cacheFile), { recursive: true });
  writeFileSync(cacheFile, JSON.stringify({ fingerprint }, null, 2) + "\n", "utf-8");
}

/** Return true if a rebuild is needed for the given target binary. */
function shouldRebuild(binaryPath, fingerprint) {
  if (!existsSync(binaryPath)) return true;
  const cache = readCache();
  if (!cache?.fingerprint) return true;
  return cache.fingerprint !== fingerprint;
}

/** Build a Go binary and log progress. */
function goBuild(label, outputPath, cmdDir) {
  assertCgoReady();
  if (existsSync(outputPath)) process.stdout.write(`Replacing ${outputPath}\n`);
  execFileSync("go", ["build", "-trimpath", "-o", outputPath, cmdDir], {
    cwd: controlRoot,
    stdio: "inherit",
  });
  assertCgoBuild(outputPath, label);
  process.stdout.write(`  ${label} → ${basename(outputPath)}\n`);
}

/** Pre-build gate: refuse to compile before a stub can be written to disk.
 *  Cheaper and safer than letting `go build` overwrite a good binary with a
 *  stub and catching it after the fact. */
function assertCgoReady() {
  const cgo = (process.env.CGO_ENABLED ?? "").trim();
  if (cgo === "0") {
    throw new Error(
      "CGO_ENABLED=0, but the sidecar uses go-sqlite3 which requires CGO. " +
      "Set CGO_ENABLED=1 (and ensure gcc is on PATH) before rebuilding.",
    );
  }
  let cc;
  try { cc = execFileSync("go", ["env", "CC"], { encoding: "utf-8" }).trim(); }
  catch { cc = ""; }
  if (!cc) {
    throw new Error(
      "No C compiler resolved (go env CC is empty). go-sqlite3 requires CGO; " +
      "install gcc/mingw-w64 on PATH and set CGO_ENABLED=1 before rebuilding.",
    );
  }
  try {
    execFileSync(cc, ["--version"], { stdio: ["ignore", "ignore", "ignore"] });
  } catch {
    throw new Error(
      `C compiler "${cc}" (from go env CC) is not runnable. go-sqlite3 requires CGO; ` +
      `fix your toolchain (install gcc/mingw-w64 on PATH, set CGO_ENABLED=1) before rebuilding.`,
    );
  }
}

/** Reject stub binaries produced when CGO was off. go-sqlite3 cannot work
 *  without CGO; a stub crashes at runtime with a confusing message. This is a
 *  backstop in case the pre-build gate is bypassed (e.g. CGO_ENABLED unset but
 *  go defaults it on, yet a future Go change flips the default). */
function assertCgoBuild(outputPath, label) {
  let info = "";
  try {
    info = execFileSync("go", ["version", "-m", outputPath], { encoding: "utf-8" });
  } catch {
    throw new Error(`Built ${label} but could not read its build info from ${outputPath}.`);
  }
  const cgoLine = info.split("\n").find((l) => l.trim().startsWith("build\tCGO_ENABLED="));
  const cgoValue = cgoLine ? cgoLine.trim().split("=")[1] : "(unknown)";
  if (cgoValue !== "1") {
    throw new Error(
      `${label} was built with CGO_ENABLED=${cgoValue}, but go-sqlite3 requires CGO. ` +
      `Set CGO_ENABLED=1 and ensure a C compiler (gcc) is on PATH before rebuilding. ` +
      `See apps/desktop/scripts/build-sidecar.mjs.`,
    );
  }
}

// Prepare the only Agent config that may be bundled. It intentionally filters
// per-machine credentials from the developer env file before any build work
// begins, so a failed release build cannot leave a stale credential resource.
function prepareAgentBootstrapConfig() {
  // 发布包只允许包含公开的云端地址。注册令牌与本地 Agent 令牌均是秘密：
  // 前者须由管理员在首次启动时按需注入，后者由桌面宿主每次启动随机生成。
  const allowed = new Set(["MILEVIA_CLOUD_URL", "MILEVIA_LOCAL_URL"]);
  const values = new Map();
  if (existsSync(agentEnvSource)) {
    readFileSync(agentEnvSource, "utf8").split(/\r?\n/)
      .map((line) => line.trim())
      .filter((line) => line && !line.startsWith("#"))
      .forEach((line) => {
        const separator = line.indexOf("=");
        if (separator <= 0) return;
        const key = line.slice(0, separator).trim();
        if (allowed.has(key) && !values.has(key)) values.set(key, line.slice(separator + 1).trim());
      });
  }
  if (process.env.MILEVIA_CLOUD_URL?.trim()) {
    values.set("MILEVIA_CLOUD_URL", process.env.MILEVIA_CLOUD_URL.trim());
  }
  const configured = Boolean(values.get("MILEVIA_CLOUD_URL"));
  mkdirSync(dirname(agentConfigOutput), { recursive: true });
  if (!configured) {
    writeFileSync(agentConfigOutput, "# Configure MILEVIA_CLOUD_URL for remote Agent enrollment\n", "utf8");
    if (process.env.MILEVIA_ALLOW_UNCONFIGURED_AGENT !== "1") {
      throw new Error(
        "Release Agent configuration requires MILEVIA_CLOUD_URL. " +
        "Provide MILEVIA_AGENT_ENROLLMENT_TOKEN only to the first local launch; " +
        "set MILEVIA_ALLOW_UNCONFIGURED_AGENT=1 only for local development.",
      );
    }
    return;
  }
  const lines = [...values.entries()].map(([key, value]) => `${key}=${value}`);
  writeFileSync(agentConfigOutput, `${lines.join("\n")}\n`, "utf8");
}

// ── Main ────────────────────────────────────────────────────────────────────

const goSources = collectFiles(controlRoot, new Set([".go"]));
const agentSources = collectFiles(agentRoot, new Set([".go"]));
// go.mod / go.sum affect dependency resolution — must be part of fingerprint
const modFiles = [
  resolve(controlRoot, "go.mod"), resolve(controlRoot, "go.sum"),
  resolve(agentRoot, "go.mod"), resolve(agentRoot, "go.sum"),
];
const agentEnvSource = resolve(agentRoot, ".env.windows");
const fingerprint = computeFingerprint([
  ...goSources,
  ...agentSources,
  ...modFiles,
  ...(existsSync(agentEnvSource) ? [agentEnvSource] : []),
]);

prepareAgentBootstrapConfig();

const controlNeeded = force || shouldRebuild(controlOutput, fingerprint);
const approvalNeeded = force || shouldRebuild(approvalOutput, fingerprint);
const terminalBridgeNeeded = force || shouldRebuild(terminalBridgeOutput, fingerprint);
const agentNeeded = force || shouldRebuild(agentOutput, fingerprint);

if (!controlNeeded && !approvalNeeded && !terminalBridgeNeeded && !agentNeeded && existsSync(agentConfigOutput)) {
  process.stdout.write("Sidecar binaries are up to date (use --force to rebuild).\n");
  process.exit(0);
}

if (force) process.stdout.write("Force-rebuilding sidecar binaries…\n");

mkdirSync(dirname(controlOutput), { recursive: true });

if (controlNeeded) goBuild("control-server", controlOutput, "./cmd/control-server");
if (approvalNeeded) goBuild("approval-helper", approvalOutput, "./cmd/approval-helper");
if (terminalBridgeNeeded) goBuild("terminal bridge", terminalBridgeOutput, "./cmd/terminal-bridge");

if (agentNeeded) {
  assertCgoReady();
  execFileSync("go", ["build", "-trimpath", "-o", agentOutput, "./cmd/milevia-agent"], {
    cwd: agentRoot,
    stdio: "inherit",
  });
  assertCgoBuild(agentOutput, "milevia-agent");
  process.stdout.write(`  agent -> ${basename(agentOutput)}\n`);
}

writeCache(fingerprint);
