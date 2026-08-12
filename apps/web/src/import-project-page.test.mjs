import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const page = await readFile(new URL("./pages/ImportProjectPage.tsx", import.meta.url), "utf8");

test("Windows-mounted paths are displayed as Windows paths while request paths remain unchanged", () => {
  assert.match(page, /function displayPath\(path: string\): string/);
  assert.match(page, /\^\(\[a-zA-Z\]\):\[\\\\\/\]\?\$/);
  assert.match(page, /\^\\\/mnt\\\/\(\[a-zA-Z\]\)\(\?:\\\/\(\.\*\)\)\?\$/);
  assert.match(page, /return rest \? `\$\{driveLabel\}.*rest\.replaceAll\("\/", "\\\\"\)\}` : driveLabel;/);
  assert.match(page, /<small>\{root\.drives \? root\.drives\.map\(\(drive\) => displayPath\(drive\.path\)\)\.join\("、"\) : displayPath\(root\.path\)\}<\/small>/);
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

test("multiple Windows drive roots merge into one card with a drive-picker step", () => {
  assert.match(page, /const displayRoots = useMemo<DisplayRootEntry\[\]>\(\(\) =>/);
  assert.match(page, /root\.label === "windows"/);
  assert.match(page, /windows\.length >= 2/);
  assert.match(page, /const \[pickingDrives, setPickingDrives\] = useState<RootEntry\[\] \| null>\(null\);/);
  assert.match(page, /root\.drives \? setPickingDrives\(root\.drives\) : selectRoot\(root\.path, root\.runnerId\)/);
  assert.match(page, /pickingDrives\.map\(\(drive\) =>/);
  assert.match(page, /setPickingDrives\(null\); selectRoot\(drive\.path, drive\.runnerId\)/);
});

test("grouped Windows card shows drive letters and a dedicated action label", () => {
  assert.match(page, /root\.drives \? root\.drives\.map\(\(drive\) => displayPath\(drive\.path\)\)\.join\("、"\) : displayPath\(root\.path\)/);
  assert.match(page, /root\.drives \? "选择盘符" : "浏览"/);
  assert.match(page, /name: "Windows"/);
  assert.match(page, /drives: windows/);
});
