/**
 * 代码字号调节 Hook — 记忆到 localStorage，跨查看器/编辑器共享
 */

import { useCallback, useEffect, useState } from "react";

export const CODE_FONT_SIZE_STORAGE_KEY = "files:code-font-size";
const MIN_SIZE = 12;
const MAX_SIZE = 20;
const DEFAULT_SIZE = 13;

function normalizeCodeFontSize(value: number): number {
  return Number.isFinite(value) ? Math.min(Math.max(Math.round(value), MIN_SIZE), MAX_SIZE) : DEFAULT_SIZE;
}

export function readCodeFontSize(): number {
  if (typeof window === "undefined") return DEFAULT_SIZE;
  try {
    const parsed = Number.parseInt(window.localStorage.getItem(CODE_FONT_SIZE_STORAGE_KEY) ?? "", 10);
    return Number.isFinite(parsed) && parsed >= MIN_SIZE && parsed <= MAX_SIZE ? parsed : DEFAULT_SIZE;
  } catch {
    return DEFAULT_SIZE;
  }
}

export function setCodeFontSize(value: number): number {
  const fontSize = normalizeCodeFontSize(value);
  if (typeof window === "undefined") return fontSize;
  try {
    window.localStorage.setItem(CODE_FONT_SIZE_STORAGE_KEY, String(fontSize));
  } catch {
    // localStorage 不可用时保持当前会话可用。
  }
  return fontSize;
}

export function useCodeFontSize() {
  const [fontSize, setFontSize] = useState<number>(readCodeFontSize);

  useEffect(() => {
    setCodeFontSize(fontSize);
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
