// 更新清单里 android 段的解析与版本比较。
//
// 单独成文件、不 import 任何 Capacitor 相关模块，是为了让它能被直接单测：
// 清单是一份会被手工编辑、也会被部署脚本改写的 JSON，"字段缺失或 url 写成相对路径"
// 这类问题必须在打包前就发现，而不是等用户点了「立即更新」才发现按钮指向错误地址。

export type AndroidRelease = {
  version: string;
  versionCode: number;
  notes?: string;
  url: string;
  size?: number;
  sha256?: string;
};

/**
 * 校验并取出 android 段。任何一项不合法都返回 null —— 宁可漏报更新，
 * 也不要让用户装上一个装不上、或指向错误地址的包。
 */
export function parseAndroidRelease(raw: unknown): AndroidRelease | null {
  if (!raw || typeof raw !== "object") return null;
  const value = raw as Record<string, unknown>;

  const version = typeof value.version === "string" ? value.version.trim() : "";
  if (!version) return null;

  const versionCode = Number(value.versionCode);
  if (!Number.isFinite(versionCode) || versionCode <= 0) return null;

  // 只接受绝对 https 地址：相对路径在 WebView 里会解析成应用自身的 origin，
  // 点下去只会白屏。
  const url = typeof value.url === "string" ? value.url.trim() : "";
  if (!/^https:\/\/\S+$/i.test(url)) return null;

  const size = Number(value.size);
  const sha256 = typeof value.sha256 === "string" ? value.sha256.trim() : "";

  return {
    version,
    versionCode,
    url,
    notes: typeof value.notes === "string" && value.notes.trim() ? value.notes.trim() : undefined,
    size: Number.isFinite(size) && size > 0 ? size : undefined,
    sha256: /^[0-9a-f]{64}$/i.test(sha256) ? sha256 : undefined,
  };
}

/**
 * 清单里是否提供了比当前安装版本更新的包。
 *
 * 比较依据是 versionCode 而不是版本号字符串：Android 自己就认 versionCode，
 * 而字符串比较会把 "0.10.0" 判成比 "0.9.9" 旧。当前版本读不出来时返回 false，
 * 不做提示。
 */
export function isNewerRelease(release: AndroidRelease | null, currentVersionCode: number): release is AndroidRelease {
  if (!release) return false;
  if (!Number.isFinite(currentVersionCode)) return false;
  return release.versionCode > currentVersionCode;
}

/** 取一份清单的原始内容（抛错表示这个源不可用）。注入进来是为了能直接单测下面的多源逻辑。 */
export type ManifestFetcher = (url: string) => Promise<unknown>;

/** 主源失败时回退到备源：自建服务器与 GitHub Pages 各一张清单，地址不同（见发布脚本）。 */
export type UpdateSourceOptions = {
  currentVersionCode: number;
  manifestUrls: readonly string[];
  fetchManifest: ManifestFetcher;
  /** 请求已被取消时返回 true，用于立刻停止而不是继续试下一个源。 */
  isAborted?: () => boolean;
};

/**
 * 依次向各个更新源要清单，返回"比当前版本新"的那条发布记录；都没有则 null。
 *
 * 两条规则：
 *  - 某一份清单**能读到**就足以定论：它说没有更新，就不再问备用源（否则主源与备源
 *    版本不一致时会来回反复）。
 *  - 所有源都读不到才抛错；中途被取消则立刻抛出，不再试下一个。
 */
export async function resolveAndroidRelease({
  currentVersionCode,
  manifestUrls,
  fetchManifest,
  isAborted,
}: UpdateSourceOptions): Promise<AndroidRelease | null> {
  let lastError: unknown = null;
  for (const url of manifestUrls) {
    let manifest: { platforms?: Record<string, unknown> } | null;
    try {
      manifest = (await fetchManifest(url)) as { platforms?: Record<string, unknown> } | null;
    } catch (error) {
      lastError = error;
      if (isAborted?.()) throw error;
      continue;
    }
    const release = parseAndroidRelease(manifest?.platforms?.android);
    return isNewerRelease(release, currentVersionCode) ? release : null;
  }
  throw lastError instanceof Error ? lastError : new Error("更新清单不可用");
}
