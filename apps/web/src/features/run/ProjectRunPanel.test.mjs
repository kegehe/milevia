import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./ProjectRunPanel.tsx", import.meta.url), "utf8");

test("refreshes uptime every second only while the project process is running", () => {
  assert.match(source, /const \[now, setNow\] = useState\(\(\) => Date\.now\(\)\);/);
  assert.match(source, /if \(status\?\.status !== "running" \|\| !status\.startedAt\) return;/);
  assert.match(source, /const refresh = \(\) => setNow\(Date\.now\(\)\);[\s\S]*?const timer = setInterval\(refresh, 1_000\);[\s\S]*?return \(\) => clearInterval\(timer\);/);
  assert.match(source, /formatUptime\(new Date\(status\.startedAt\), now\)/);
});

test("commits an environment variable rename when focus leaves the row, not on every keystroke", () => {
  // 键名框原先逐键改写变量表，`next[e.target.value || k] = v` 有两个毛病：删到最后一个字符会
  // 回落成旧键（框里永远清不空），改成已存在的键名会静默覆盖那一条的取值。判重与空键名归
  // renameEnvironmentVariable（见 run-model.test.ts），这里只钉住界面把决定权交给它的方式。
  assert.doesNotMatch(source, /e\.target\.value \|\| k/);
  assert.match(source, /renameEnvironmentVariable\(envVars, oldKey, draft\.value\)/);

  // 键名框留草稿、离开整行时才提交；焦点只在这一行内换框（键名 → 值）时不提交 —— 提交会重建
  // 这一行，把刚点进去的框连焦点一起换掉。
  assert.match(source, /className="run-env-var" onBlur=\{\(e\) => \{/);
  assert.match(source, /e\.currentTarget\.contains\(target\)/);
  assert.match(source, /commitRename\(k\);/);

  // 键名被占用时给出提示，而不是默默改掉别人的变量。
  assert.match(source, /已存在，未改名/);
  assert.match(source, /renameError \? <p className="run-env-error" role="status">\{renameError\}<\/p> : null/);
});
