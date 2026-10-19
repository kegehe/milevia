type Request = (path: string, init: RequestInit, token?: string) => Promise<unknown>;

export async function revokeMobileDevice(request: Request, instanceID: string, token?: string) {
  if (!instanceID.trim()) return;
  await request(
    `/v1/instances/${encodeURIComponent(instanceID.trim())}/revoke`,
    { method: "POST", body: JSON.stringify({ scope: "mobile" }) },
    token,
  );
}
