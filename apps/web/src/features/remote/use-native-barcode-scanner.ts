import { useEffect, useRef } from "react";
import { Capacitor, type PluginListenerHandle } from "@capacitor/core";
import { BarcodeFormat, BarcodeScanner, LensFacing } from "@capacitor-mlkit/barcode-scanning";

type Options = {
  scanning: boolean;
  onScan: (value: string) => boolean;
  onError: (message: string, needsSettings: boolean) => void;
  onTimeout: () => void;
  onTorchAvailability: (available: boolean) => void;
  onStopped: () => void;
};

export function useNativeBarcodeScanner({ scanning, onScan, onError, onTimeout, onTorchAvailability, onStopped }: Options) {
  const callbacksRef = useRef({ onScan, onError, onTimeout, onTorchAvailability, onStopped });
  callbacksRef.current = { onScan, onError, onTimeout, onTorchAvailability, onStopped };

  useEffect(() => {
    if (!scanning || !Capacitor.isNativePlatform()) return;
    let cancelled = false;
    let started = false;
    let timeout: number | undefined;
    let listener: PluginListenerHandle | null = null;
    const stop = async () => {
      if (timeout !== undefined) window.clearTimeout(timeout);
      await listener?.remove().catch(() => undefined);
      listener = null;
      if (started) await BarcodeScanner.stopScan().catch(() => undefined);
      callbacksRef.current.onStopped();
    };
    void (async () => {
      try {
        const { supported } = await BarcodeScanner.isSupported();
        if (!supported) throw new Error("此设备没有可用摄像头");
        let permission = await BarcodeScanner.checkPermissions();
        if (permission.camera === "prompt" || permission.camera === "prompt-with-rationale") permission = await BarcodeScanner.requestPermissions();
        if (permission.camera !== "granted" && permission.camera !== "limited") {
          callbacksRef.current.onError("未获得摄像头权限，请在系统设置中允许 Milevia 使用摄像头", true);
          return;
        }
        if (cancelled) return;
        listener = await BarcodeScanner.addListener("barcodesScanned", ({ barcodes }) => {
          for (const barcode of barcodes) if (callbacksRef.current.onScan(barcode.rawValue || barcode.displayValue)) return;
        });
        if (cancelled) { await stop(); return; }
        await BarcodeScanner.startScan({ formats: [BarcodeFormat.QrCode], lensFacing: LensFacing.Back });
        started = true;
        if (cancelled) { await stop(); return; }
        void BarcodeScanner.isTorchAvailable()
          .then((value) => {
            if (!cancelled) callbacksRef.current.onTorchAvailability(Boolean((value as { available?: boolean })?.available));
          })
          .catch(() => undefined);
        timeout = window.setTimeout(() => { if (!cancelled) callbacksRef.current.onTimeout(); }, 30_000);
      } catch (cause) {
        if (!cancelled) callbacksRef.current.onError(cause instanceof Error ? cause.message : "无法启动二维码扫描，请稍后重试", false);
      }
    })();
    return () => { cancelled = true; void stop(); };
  }, [scanning]);
}
