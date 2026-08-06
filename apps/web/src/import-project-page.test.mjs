import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("./pages/ImportProjectPage.tsx", import.meta.url), "utf8");

test("Windows-mounted paths are displayed as Windows paths while request paths remain unchanged", () => {
  assert.match(page, /function displayPath\(path: string\): string/);
  assert.match(page, /\^\\\/mnt\\\/\(\[a-zA-Z\]\)\(\?:\\\/\(\.\*\)\)\?\$/);
  assert.match(page, /return `\$\{drive\.toUpperCase\(\)\}:\\\\\$\{rest\.replaceAll\("\/", "\\\\"\)\}`;/);
  assert.match(page, /<small>\{displayPath\(root\.path\)\}<\/small>/);
  assert.match(page, /<code>\{displayPath\(path\)\}<\/code>/);
  assert.match(page, /<small>\{displayPath\(dir\.path\)\}<\/small>/);
  assert.match(page, /void browse\(dir\.path\)/);
});

test("the initial root browse uses the selected runner instead of stale React state", () => {
  assert.match(page, /const browse = useCallback\(async \(target = "", runnerID = activeRunner\) =>/);
  assert.match(page, /const runnerParam = runnerID \? `\$\{query \? "&" : "\?"\}runner=\$\{encodeURIComponent\(runnerID\)\}` : "";/);
  assert.match(page, /void browse\(rootPath, runnerId \|\| ""\);/);
});

test("directory browsing errors remain visible in the import dialog", () => {
  assert.match(page, /const \[localError, setLocalError\] = useState<string \| null>\(null\);/);
  assert.match(page, /setLocalError\(cause instanceof Error \? cause\.message : "无法浏览目录"\)/);
  assert.match(page, /\{localError && <p className="error" role="alert">\{localError\}<\/p>\}/);
});

test("only the newest directory request can update the import view", () => {
  assert.match(page, /const browseRequestRef = useRef\(0\);/);
  assert.match(page, /const requestID = \+\+browseRequestRef\.current;/);
  assert.match(page, /requestID !== browseRequestRef\.current/);
  assert.match(page, /browseRequestRef\.current\+\+;/);
  assert.match(page, /setPath\(target\);\s*setParent\(""\);\s*setDirs\(\[\]\);\s*setResult\(null\);/);
});

test("directory-dependent actions wait for directory loading", () => {
  assert.match(page, /const \[loadingDirectory, setLoadingDirectory\] = useState\(false\);/);
  assert.match(page, /const \[directoryReady, setDirectoryReady\] = useState\(false\);/);
  assert.match(page, /disabled=\{busy \|\| loadingDirectory \|\| creatingDirectory \|\| !directoryReady\}/);
  assert.match(page, /disabled=\{!result\?\.agentReady \|\| busy \|\| loadingDirectory \|\| !directoryReady\}/);
});

test("validation and project creation cannot update a stale import view", () => {
  assert.match(page, /const validationRequestRef = useRef\(0\);/);
  assert.match(page, /const requestID = \+\+validationRequestRef\.current;/);
  assert.match(page, /requestID === validationRequestRef\.current\) setResult\(validationResult\)/);
  assert.match(page, /const createRequestRef = useRef\(0\);/);
  assert.match(page, /requestID === createRequestRef\.current\) navigate\(`/);
  assert.match(page, /disabled=\{busy\} onClick=\{returnToRootSelection\}/);
});
