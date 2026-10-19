import { useCallback, useRef } from "react";
import { claimMobilePairing, type MobilePairingClaim } from "./mobile-pairing-request";

type Request = <T>(path: string, init: RequestInit) => Promise<T>;

export function useMobilePairingClaim(options: {
  request: Request;
  device: () => { deviceName: string; platform: string };
  setBusy: (busy: boolean) => void;
  clearError: () => void;
  reportError: (message: string) => void;
}) {
  const optionsRef = useRef(options);
  optionsRef.current = options;

  return useCallback(async (input: {
    pairingID?: string;
    code: string;
    validationError: string;
    failureError: string;
  }): Promise<MobilePairingClaim | null> => {
    const current = optionsRef.current;
    if (!/^\d{6}$/.test(input.code.trim()) || (input.pairingID !== undefined && !input.pairingID.trim())) {
      current.reportError(input.validationError);
      return null;
    }
    current.setBusy(true);
    current.clearError();
    try {
      return await claimMobilePairing(current.request, current.device(), { pairingID: input.pairingID, code: input.code });
    } catch (cause) {
      current.reportError(cause instanceof Error ? cause.message : input.failureError);
      return null;
    } finally {
      current.setBusy(false);
    }
  }, []);
}
