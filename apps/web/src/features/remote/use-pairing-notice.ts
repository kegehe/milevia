import { useEffect, useState } from "react";

export type PairingNoticeState = "idle" | "waiting" | "success" | "error";
export type PairingNotice = { text: string; state: PairingNoticeState };

export function usePairingNotice() {
  const [notice, setNotice] = useState<PairingNotice | null>(null);

  useEffect(() => {
    if (notice?.state !== "success") return;
    const timer = window.setTimeout(() => setNotice(null), 8000);
    return () => window.clearTimeout(timer);
  }, [notice]);

  return { notice, setNotice };
}
