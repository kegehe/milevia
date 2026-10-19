import { useCallback, useRef, useState } from "react";
import { useDesktopPairingStatusPoll } from "./use-pairing-status-polls";

type Request = <T>(path: string, init?: RequestInit) => Promise<T>;

type PairingResponse = {
  pairingId?: string;
  code?: string;
  pairingURL?: string;
  expiresAt?: string;
};

export function useDesktopPairing(options: {
  enabled: boolean;
  request: Request;
  setBusy: (busy: boolean) => void;
  clearError: () => void;
  reportError: (message: string) => void;
  createPairingURL: (value: string | undefined, pairingID: string, code: string) => string;
  onNotice: (notice: { text: string; state: "idle" | "waiting" | "success" | "error" }) => void;
  onConfirmed: () => void;
  onGenerated: () => void;
  onCreateFailure: () => void;
  initialPairingID?: string;
  initialCode?: string;
}) {
  const [pairingID, setPairingID] = useState(options.initialPairingID || "");
  const [pairingCode, setPairingCode] = useState(options.initialCode || "");
  const [pairingURL, setPairingURL] = useState("");
  const [pairingExpiresAt, setPairingExpiresAt] = useState("");
  const [pairingReadyForConfirm, setPairingReadyForConfirm] = useState(false);
  const [pairingConfirmed, setPairingConfirmed] = useState(false);
  const callbacksRef = useRef(options);
  callbacksRef.current = options;

  const resetPairing = useCallback(() => {
    setPairingID("");
    setPairingCode("");
    setPairingURL("");
    setPairingExpiresAt("");
    setPairingReadyForConfirm(false);
    setPairingConfirmed(false);
  }, []);

  const complete = useCallback(() => {
    setPairingConfirmed(true);
    setPairingReadyForConfirm(false);
    callbacksRef.current.onNotice({ text: "已确认绑定，手机可以开始使用", state: "success" });
    callbacksRef.current.onConfirmed();
  }, []);

  const createDesktopPairing = useCallback(async () => {
    const current = callbacksRef.current;
    current.setBusy(true);
    current.clearError();
    try {
      const value = await current.request<PairingResponse>("/api/remote/pairing", { method: "POST" });
      const nextPairingID = value.pairingId || "";
      const nextCode = value.code || "";
      const nextURL = current.createPairingURL(value.pairingURL, nextPairingID, nextCode);
      setPairingID(nextPairingID);
      setPairingCode(nextCode);
      setPairingURL(nextURL);
      setPairingExpiresAt(value.expiresAt || "");
      setPairingReadyForConfirm(false);
      setPairingConfirmed(false);
      current.onGenerated();
      if (nextURL) {
        current.onNotice({ text: "二维码已生成，请用手机扫码；在本机点击「确认绑定」后完成", state: "idle" });
      } else {
        current.onNotice({ text: "已生成校验码，请在手机上输入这 6 位数字", state: "idle" });
        current.reportError("云端未配置公网地址（MILEVIA_CLOUD_APP_URL），二维码不可用；请改用校验码配对。");
      }
    } catch (cause) {
      current.reportError(cause instanceof Error ? cause.message : "无法生成配对二维码");
      current.onCreateFailure();
    } finally {
      current.setBusy(false);
    }
  }, []);

  const confirmDesktopPairing = useCallback(async () => {
    const current = callbacksRef.current;
    const target = pairingID.trim();
    if (!target) return;
    current.setBusy(true);
    current.clearError();
    try {
      await current.request("/api/remote/pairing/confirm", { method: "POST", body: JSON.stringify({ pairingId: target }) });
      complete();
    } catch (cause) {
      current.reportError(cause instanceof Error ? cause.message : "确认绑定失败");
    } finally {
      current.setBusy(false);
    }
  }, [complete, pairingID]);

  useDesktopPairingStatusPoll({
    enabled: options.enabled,
    pairingID,
    confirmed: pairingConfirmed,
    request: options.request,
    onStatus: (status) => {
      if (status === "confirmed") complete();
      else if (status === "expired" || status === "cancelled") {
        setPairingReadyForConfirm(false);
        callbacksRef.current.onNotice({ text: "配对已失效，请重新生成二维码", state: "error" });
      } else if (status === "claimed") {
        setPairingReadyForConfirm(true);
        callbacksRef.current.onNotice({ text: "手机已扫码提交，请点击「确认绑定」", state: "waiting" });
      } else setPairingReadyForConfirm(false);
    },
  });

  return {
    pairingID,
    setPairingID,
    pairingCode,
    setPairingCode,
    pairingURL,
    pairingExpiresAt,
    pairingReadyForConfirm,
    pairingConfirmed,
    createDesktopPairing,
    confirmDesktopPairing,
    resetPairing,
  };
}
