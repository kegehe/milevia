export type MobilePairingClaim = { pairingId?: string; instanceId: string; status: string; accessToken?: string };

type Request = <T>(path: string, init: RequestInit) => Promise<T>;
type Device = { deviceName: string; platform: string };

export async function claimMobilePairing(
  request: Request,
  device: Device,
  input: { pairingID?: string; code: string },
): Promise<MobilePairingClaim> {
  const code = input.code.trim();
  if (!/^\d{6}$/.test(code)) throw new Error("请输入 6 位校验码");
  const body = JSON.stringify({ code, deviceName: device.deviceName, platform: device.platform });
  if (input.pairingID?.trim()) {
    return request<MobilePairingClaim>(`/v1/pairings/${encodeURIComponent(input.pairingID.trim())}/claim`, { method: "POST", body });
  }
  return request<MobilePairingClaim>("/v1/pairings/claim", { method: "POST", body });
}
