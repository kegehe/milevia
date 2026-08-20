import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./FileViewer.tsx", import.meta.url), "utf8");

test("desktop media and downloads use the authenticated runtime transport", () => {
  assert.match(source, /import \{ apiURL, isDesktop, sessionHeaders \}/);
  assert.match(source, /const \[source, setSource\] = useState<string \| null>\(\(\) => desktop \? null : apiURL\(url\)\)/);
  assert.match(source, /setSource\(null\);/);
  assert.match(source, /download-ticket/);
  assert.doesNotMatch(source, /const objectURL = URL\.createObjectURL\(await response\.blob\(\)\)/);
  assert.match(source, /<ProjectImage url=\{url\}/);
  assert.match(source, /<DownloadLink url=\{url\} filename=\{stat\.name\}/);
  assert.match(source, /const handleNotDatabase = useCallback/);
  assert.match(source, /const \[imageErrorPath, setImageErrorPath\] = useState<string \| null>\(null\)/);
  assert.match(source, /const \[sqliteInvalidPath, setSqliteInvalidPath\] = useState<string \| null>\(null\)/);
  assert.match(source, /const sqliteInvalid = sqliteInvalidPath === stat\.path/);
});
