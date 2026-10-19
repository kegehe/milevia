import { useEffect, useState } from "react";
import QRCode from "qrcode";

export function secondsUntil(value: string | undefined): number {
  const deadline = Date.parse(value || "");
  return Number.isFinite(deadline) ? Math.max(0, Math.round((deadline - Date.now()) / 1000)) : 0;
}

export function usePairingPresentation(pairingURL: string, expiresAt: string) {
  const [pairingQR, setPairingQR] = useState("");
  const [secondsLeft, setSecondsLeft] = useState(() => secondsUntil(expiresAt));

  useEffect(() => {
    let cancelled = false;
    if (!pairingURL) { setPairingQR(""); return () => { cancelled = true; }; }
    void QRCode.toDataURL(pairingURL, { width: 240, margin: 1, errorCorrectionLevel: "M" })
      .then((dataURL) => { if (!cancelled) setPairingQR(dataURL); })
      .catch(() => { if (!cancelled) setPairingQR(""); });
    return () => { cancelled = true; };
  }, [pairingURL]);

  useEffect(() => {
    const deadline = Date.parse(expiresAt);
    if (!Number.isFinite(deadline)) { setSecondsLeft(0); return; }
    const tick = () => setSecondsLeft(Math.max(0, Math.round((deadline - Date.now()) / 1000)));
    tick();
    const timer = window.setInterval(tick, 1000);
    return () => window.clearInterval(timer);
  }, [expiresAt]);

  return { pairingQR, secondsLeft };
}
