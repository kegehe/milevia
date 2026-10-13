import { useCallback, useEffect, useState } from "react";

export const ERROR_NOTICE_DURATION_MS = 6000;

/** 全局操作错误短暂显示；每次报错都重置计时，包括内容相同的错误。 */
export function useTransientError() {
  const [notice, setNotice] = useState({ message: "" });
  const setError = useCallback((message: string) => {
    setNotice({ message });
  }, []);

  useEffect(() => {
    if (!notice.message) return;
    const timer = window.setTimeout(() => {
      // 旧计时器即使已进入任务队列，也不能清除后来收到的错误。
      setNotice((current) => current === notice ? { message: "" } : current);
    }, ERROR_NOTICE_DURATION_MS);
    return () => window.clearTimeout(timer);
  }, [notice]);

  return [notice.message, setError] as const;
}
