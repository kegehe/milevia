import { existsSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { execFileSync } from "node:child_process";

const desktopRoot = resolve(import.meta.dirname, "..");
const controlRoot = resolve(desktopRoot, "../control-server");
const binaries = resolve(desktopRoot, "binaries");
const controlOutput = resolve(binaries, "milevia-control.exe");
const approvalOutput = resolve(binaries, "milevia-approval.exe");

if (process.platform !== "win32") {
  throw new Error("Windows desktop sidecar must be built on Windows so the CGO SQLite toolchain is reproducible.");
}

mkdirSync(dirname(controlOutput), { recursive: true });
if (existsSync(controlOutput)) process.stdout.write(`Replacing ${controlOutput}\n`);
execFileSync("go", ["build", "-trimpath", "-o", controlOutput, "./cmd/control-server"], {
  cwd: controlRoot,
  stdio: "inherit",
});
if (existsSync(approvalOutput)) process.stdout.write(`Replacing ${approvalOutput}\n`);
execFileSync("go", ["build", "-trimpath", "-o", approvalOutput, "./cmd/approval-helper"], {
  cwd: controlRoot,
  stdio: "inherit",
});
