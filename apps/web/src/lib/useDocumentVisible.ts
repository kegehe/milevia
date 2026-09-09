import { useEffect, useState } from "react";

// 页面可见性：窗口最小化或切到后台标签页时返回 false。
// 用于给轮询（setInterval）加闸，避免在不可见时持续拉取接口浪费请求与电量。
export function useDocumentVisible(): boolean {
  const [visible, setVisible] = useState(() => document.visibilityState !== "hidden");
  useEffect(() => {
    const onChange = () => setVisible(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", onChange);
    return () => document.removeEventListener("visibilitychange", onChange);
  }, []);
  return visible;
}