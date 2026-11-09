import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { sourceFingerprint } from "./build-supervision-targets.mjs";

test("嵌入的认证脚本变化必须使目标构建缓存失效", (t) => {
  const root = mkdtempSync(join(tmpdir(), "milevia-build-inputs-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const dir = join(root, "internal", "app");
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(root, "go.mod"), "module fixture\n");
  writeFileSync(join(dir, "auth.go"), 'package app\nimport _ "embed"\n//go:embed codebuddy_auth_probe.cjs\nvar script string\n');
  const script = join(dir, "codebuddy_auth_probe.cjs");
  writeFileSync(script, "version one");
  const before = sourceFingerprint("go fixture", root);
  writeFileSync(script, "version two");
  assert.notEqual(sourceFingerprint("go fixture", root), before);
  const after = sourceFingerprint("go fixture", root);
  writeFileSync(join(dir, "auth_test.go"), "test changes alone");
  assert.equal(sourceFingerprint("go fixture", root), after);
});

test("桌面二进制构建同样跟踪嵌入脚本", () => {
  const sidecar = readFileSync(new URL("../apps/desktop/scripts/build-sidecar.mjs", import.meta.url), "utf8");
  assert.match(sidecar, /collectFiles\(controlRoot, new Set\(\[[^\]]*"\.cjs"/);
});
