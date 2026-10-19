import type { PairingNotice } from "./use-pairing-notice";

export function noticeAfterClosingPairingPanel(notice: PairingNotice | null) {
  return notice?.state === "waiting" ? notice : null;
}

export function pairingPanelOpenState() {
  return { notice: null, manualCode: "", devicesOpen: false, expanded: true };
}
