import { createHash, randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, readdirSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const source = join(root, "apps", "control-server");
const supportedTargets = new Set(["windows-amd64", "windows-arm64", "linux-amd64", "linux-arm64"]);
const sha256 = (value) => createHash("sha256").update(value).digest("hex");

export function sourceFingerprint(goVersion, sourceRoot = source) {
  const hash = createHash("sha256").update(goVersion);
  function walk(directory) {
    for (const entry of readdirSync(directory, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      const path = join(directory, entry.name);
      if (entry.isDirectory()) walk(path);
      // CJS files are go:embed inputs (the CodeBuddy ACP auth probe). A script
      // change alone must never reuse a helper containing the old protocol.
      else if ((entry.name.endsWith(".go") && !entry.name.endsWith("_test.go")) || entry.name.endsWith(".cjs") || entry.name === "go.mod" || entry.name === "go.sum") {
        hash.update(path.slice(sourceRoot.length));
        hash.update(readFileSync(path));
      }
    }
  }
  walk(sourceRoot);
  hash.update(readFileSync(fileURLToPath(import.meta.url)));
  return hash.digest("hex");
}

// This builds authenticated stdio helpers only. They must be launched with
// --mode supervision-target; CGO=0 is intentional because this mode never opens
// the business database. Desktop/web control-server builds still require CGO.
export function buildSupervisionTargets({ out, targets = ["windows-amd64", "linux-amd64", "linux-arm64"], force = false }) {
  if (!out || !targets.length || targets.some((target) => !supportedTargets.has(target)) || new Set(targets).size !== targets.length) {
    throw new Error("Specify an output directory and unique Windows/Linux amd64/arm64 targets.");
  }
  out = resolve(out);
  mkdirSync(out, { recursive: true });
  const goVersion = execFileSync("go", ["version"], { cwd: source, encoding: "utf8" }).trim();
  const fingerprint = sourceFingerprint(goVersion);
  const manifestPath = join(out, "manifest.json");
  let prior;
  try { prior = JSON.parse(readFileSync(manifestPath, "utf8")); } catch { prior = undefined; }
  const artifacts = [];
  for (const target of targets) {
    const [os, arch] = target.split("-");
    const file = `milevia-supervision-target-${target}${os === "windows" ? ".exe" : ""}`;
    const output = join(out, file);
    const cached = prior?.artifacts?.find((item) => item.os === os && item.arch === arch && item.file === file);
    let bytes = existsSync(output) ? readFileSync(output) : undefined;
    if (force || prior?.protocol !== 1 || prior?.fingerprint !== fingerprint || !bytes || cached?.sha256 !== sha256(bytes) || cached?.size !== bytes.length) {
      const temporary = join(out, `${file}.${randomUUID()}.tmp`);
      try {
        execFileSync("go", ["build", "-p=1", "-trimpath", "-ldflags=-s -w", "-o", temporary, "./cmd/control-server"], {
          cwd: source,
          env: { ...process.env, GOOS: os, GOARCH: arch, CGO_ENABLED: "0", GOMAXPROCS: process.env.GOMAXPROCS || "4", GOMEMLIMIT: process.env.GOMEMLIMIT || "768MiB" },
          stdio: "inherit",
        });
        bytes = readFileSync(temporary);
        renameSync(temporary, output);
      } finally {
        // Delete only this build's explicitly created temporary file.
        rmSync(temporary, { force: true });
      }
    }
    const artifact = { os, arch, file, size: bytes.length, sha256: sha256(bytes) };
    artifacts.push(artifact);
    process.stdout.write(`${target}: ${artifact.size} bytes, sha256=${artifact.sha256}\n`);
  }
  const manifest = { protocol: 1, fingerprint, goVersion, launchArgs: ["--mode", "supervision-target"], artifacts };
  const temporaryManifest = join(out, `manifest.${randomUUID()}.tmp`);
  try {
    writeFileSync(temporaryManifest, JSON.stringify(manifest, null, 2) + "\n", { mode: 0o600 });
    renameSync(temporaryManifest, manifestPath);
  } finally {
    rmSync(temporaryManifest, { force: true });
  }
  return manifest;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  let out;
  let targets;
  let force = false;
  for (let index = 2; index < process.argv.length; index++) {
    const flag = process.argv[index];
    if (flag === "--out") out = process.argv[++index];
    else if (flag === "--targets") targets = process.argv[++index]?.split(",");
    else if (flag === "--force") force = true;
    else throw new Error(`Unknown option: ${flag}`);
  }
  buildSupervisionTargets({ out, targets, force });
}
