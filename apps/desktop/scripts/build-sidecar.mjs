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

/** Compute a fingerprint of tracked source/config files:
 *  path → mtime mapping, hashed with SHA-256. */
function computeFingerprint(paths) {
  const hash = createHash("sha256");
  const sorted = paths.slice().sort();
  for (const p of sorted) {
    try { hash.update(`${p}@${statSync(p).mtimeMs}`); }
    catch { /* deleted file — fingerprint will differ next time */ }
  }
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
  if (existsSync(outputPath)) process.stdout.write(`Replacing ${outputPath}\n`);
  execFileSync("go", ["build", "-trimpath", "-o", outputPath, cmdDir], {
    cwd: controlRoot,
    stdio: "inherit",
  });
  process.stdout.write(`  ${label} → ${basename(outputPath)}\n`);
}

// ── Main ────────────────────────────────────────────────────────────────────

const goSources = collectFiles(controlRoot, new Set([".go"]));
// go.mod / go.sum affect dependency resolution — must be part of fingerprint
const modFiles = [resolve(controlRoot, "go.mod"), resolve(controlRoot, "go.sum")];
const fingerprint = computeFingerprint([...goSources, ...modFiles]);

const controlNeeded = force || shouldRebuild(controlOutput, fingerprint);
const approvalNeeded = force || shouldRebuild(approvalOutput, fingerprint);

if (!controlNeeded && !approvalNeeded) {
  process.stdout.write("Sidecar binaries are up to date (use --force to rebuild).\n");
  process.exit(0);
}

if (force) process.stdout.write("Force-rebuilding sidecar binaries…\n");

mkdirSync(dirname(controlOutput), { recursive: true });

if (controlNeeded) goBuild("control-server", controlOutput, "./cmd/control-server");
if (approvalNeeded) goBuild("approval-helper", approvalOutput, "./cmd/approval-helper");

writeCache(fingerprint);
