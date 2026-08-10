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
  return <span className="app-version-tag" title={`Milevia v${version}`}>v{version}</span>;
}
