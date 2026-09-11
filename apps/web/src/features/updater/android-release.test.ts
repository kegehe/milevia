import assert from "node:assert/strict";
import test from "node:test";
import { isNewerRelease, parseAndroidRelease, resolveAndroidRelease } from "./android-release";

const valid = {
  version: "0.1.6",
  versionCode: 4,
  notes: "新增应用内检查更新",
  url: "https://keyanjia.info:8443/updates/Milevia_0.1.6_android.apk",
  size: 28505911,
  sha256: "72959671a944b0cd4319ac3788ecf7cba80860b60bc0216423cfd6663cedaa4d",
};

test("接受完整合法的 android 段", () => {
  const release = parseAndroidRelease(valid);
  assert.ok(release);
  assert.equal(release.version, "0.1.6");
  assert.equal(release.versionCode, 4);
  assert.equal(release.size, 28505911);
  assert.equal(release.sha256, valid.sha256);
});

test("缺少 platforms.android 时不报更新", () => {
  assert.equal(parseAndroidRelease(undefined), null);
  assert.equal(parseAndroidRelease(null), null);
  assert.equal(parseAndroidRelease("0.1.6"), null);
  assert.equal(isNewerRelease(null, 3), false);
});

test("url 必须是绝对 https 地址", () => {
  // 相对路径在 WebView 里会解析成应用自身的 origin，点下去只会白屏。
  assert.equal(parseAndroidRelease({ ...valid, url: "/updates/x.apk" }), null);
  assert.equal(parseAndroidRelease({ ...valid, url: "http://keyanjia.info/x.apk" }), null);
  assert.equal(parseAndroidRelease({ ...valid, url: "" }), null);
  assert.equal(parseAndroidRelease({ ...valid, url: undefined }), null);
});

test("versionCode 必须是正整数", () => {
  assert.equal(parseAndroidRelease({ ...valid, versionCode: 0 }), null);
  assert.equal(parseAndroidRelease({ ...valid, versionCode: -1 }), null);
  assert.equal(parseAndroidRelease({ ...valid, versionCode: "四" }), null);
  assert.equal(parseAndroidRelease({ ...valid, versionCode: undefined }), null);
  // 字符串数字是允许的：清单可能被手工编辑成 "4"。
  assert.equal(parseAndroidRelease({ ...valid, versionCode: "4" })?.versionCode, 4);
});

test("version 不能为空", () => {
  assert.equal(parseAndroidRelease({ ...valid, version: "   " }), null);
  assert.equal(parseAndroidRelease({ ...valid, version: undefined }), null);
});

test("可选字段缺失不影响解析", () => {
  const release = parseAndroidRelease({ version: "0.1.6", versionCode: 4, url: valid.url });
  assert.ok(release);
  assert.equal(release.notes, undefined);
  assert.equal(release.size, undefined);
  assert.equal(release.sha256, undefined);
});

test("非法 sha256 与 size 被忽略而不是照单全收", () => {
  const release = parseAndroidRelease({ ...valid, sha256: "abc", size: -5 });
  assert.ok(release);
  assert.equal(release.sha256, undefined);
  assert.equal(release.size, undefined);
});

test("versionCode 更大才算有新版本（不能用字符串比较）", () => {
  const release = parseAndroidRelease(valid);
  assert.ok(release);
  assert.equal(isNewerRelease(release, 3), true);
  assert.equal(isNewerRelease(release, 4), false);
  assert.equal(isNewerRelease(release, 5), false);
  // 0.10.0 必须被判定为比 0.9.9 新 —— 字符串比较会得出相反结论。
  const ten = parseAndroidRelease({ ...valid, version: "0.10.0", versionCode: 10 });
  const nine = parseAndroidRelease({ ...valid, version: "0.9.9", versionCode: 9 });
  assert.ok(ten && nine);
  assert.equal(isNewerRelease(ten, 9), true);
  assert.equal(isNewerRelease(nine, 10), false);
});

test("读不出版本号时不提示更新", () => {
  const release = parseAndroidRelease(valid);
  assert.equal(isNewerRelease(release, Number.NaN), false);
});

const PRIMARY = "https://keyanjia.info:8443/updates/latest.json";
const FALLBACK = "https://kegehe.github.io/milevia/latest.json";
const manifestFor = (versionCode: number) => ({ platforms: { android: { ...valid, versionCode } } });

test("主源不可达时回退到备用源", async () => {
  const tried: string[] = [];
  const release = await resolveAndroidRelease({
    currentVersionCode: 3,
    manifestUrls: [PRIMARY, FALLBACK],
    fetchManifest: async (url) => {
      tried.push(url);
      if (url === PRIMARY) throw new Error("主源连不上");
      return manifestFor(4);
    },
  });
  assert.deepEqual(tried, [PRIMARY, FALLBACK]);
  assert.equal(release?.versionCode, 4);
});

test("主源能读到就以它为准，不再问备用源", async () => {
  const tried: string[] = [];
  const release = await resolveAndroidRelease({
    currentVersionCode: 3,
    manifestUrls: [PRIMARY, FALLBACK],
    fetchManifest: async (url) => {
      tried.push(url);
      return manifestFor(4);
    },
  });
  assert.deepEqual(tried, [PRIMARY]);
  assert.equal(release?.versionCode, 4);
});

test("主源说没有更新时也不去问备用源", async () => {
  // 否则两张清单版本不一致时会来回反复，用户看到提示忽闪。
  const tried: string[] = [];
  const release = await resolveAndroidRelease({
    currentVersionCode: 4,
    manifestUrls: [PRIMARY, FALLBACK],
    fetchManifest: async (url) => {
      tried.push(url);
      return manifestFor(4);
    },
  });
  assert.deepEqual(tried, [PRIMARY]);
  assert.equal(release, null);
});

test("所有源都不可达时抛错", async () => {
  await assert.rejects(
    resolveAndroidRelease({
      currentVersionCode: 3,
      manifestUrls: [PRIMARY, FALLBACK],
      fetchManifest: async () => {
        throw new Error("网络不可用");
      },
    }),
    /网络不可用/,
  );
});

test("请求已取消时立刻抛出，不再试备用源", async () => {
  const tried: string[] = [];
  await assert.rejects(
    resolveAndroidRelease({
      currentVersionCode: 3,
      manifestUrls: [PRIMARY, FALLBACK],
      fetchManifest: async (url) => {
        tried.push(url);
        throw new Error("aborted");
      },
      isAborted: () => true,
    }),
    /aborted/,
  );
  assert.deepEqual(tried, [PRIMARY]);
});

test("备用源的清单里没有 android 段时不提示更新", async () => {
  // 备用源在拿不到 GitHub 资产时会写成"只报版本、不提供下载"，此时应当安静地不提示。
  const release = await resolveAndroidRelease({
    currentVersionCode: 3,
    manifestUrls: [PRIMARY, FALLBACK],
    fetchManifest: async (url) => {
      if (url === PRIMARY) throw new Error("主源连不上");
      return { version: "0.1.6", platforms: {} };
    },
  });
  assert.equal(release, null);
});
