/**
 * 代码字号调节 Hook — 记忆到 localStorage，跨查看器/编辑器共享
 */

import { useCallback, useEffect, useState } from "react";

const STORAGE_KEY = "files:code-font-size";
const MIN_SIZE = 12;
const MAX_SIZE = 20;
const DEFAULT_SIZE = 13;

export function useCodeFontSize() {
  const [fontSize, setFontSize] = useState<number>(() => {
    if (typeof window === "undefined") return DEFAULT_SIZE;
    const stored = window.localStorage.getItem(STORAGE_KEY);
    const parsed = stored ? Number.parseInt(stored, 10) : NaN;
    if (Number.isFinite(parsed) && parsed >= MIN_SIZE && parsed <= MAX_SIZE) {
      return parsed;
    }
    return DEFAULT_SIZE;
  });

  useEffect(() => {
    try {
      window.localStorage.setItem(STORAGE_KEY, String(fontSize));
    } catch {
      // 忽略写入失败（如隐私模式）
    }
  }, [fontSize]);

  const increase = useCallback(() => {
    setFontSize((s) => Math.min(s + 1, MAX_SIZE));
  }, []);

  const decrease = useCallback(() => {
    setFontSize((s) => Math.max(s - 1, MIN_SIZE));
  }, []);

  const reset = useCallback(() => {
    setFontSize(DEFAULT_SIZE);
  }, []);

  return {
    fontSize,
    increase,
    decrease,
    reset,
    canIncrease: fontSize < MAX_SIZE,
    canDecrease: fontSize > MIN_SIZE,
  };
}

export const CODE_FONT_SIZE_BOUNDS = { min: MIN_SIZE, max: MAX_SIZE, default: DEFAULT_SIZE };
