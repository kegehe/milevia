// 主界面角落的应用版本号（桌面端）。浏览器环境不显示。

import { useEffect, useState } from "react";
import { getVersion } from "@tauri-apps/api/app";
import { isDesktop } from "../../lib/runtime";

export function AppVersionTag() {
  const [version, setVersion] = useState<string | null>(null);

  useEffect(() => {
    if (!isDesktop()) return;
    getVersion()
      .then(setVersion)
      .catch(() => setVersion(null));
  }, []);

  if (!isDesktop() || !version) return null;
  return (
    <span className="app-version-tag" title={`Milevia v${version}`}>
      {/* 复用品牌 logo（milevia-mark.svg），与顶部品牌栏同一份资源，避免图形漂移 */}
      <img className="app-version-mark" src="/milevia-mark.svg" alt="" />
      <span className="app-version-text">v{version}</span>
    </span>
  );
}
