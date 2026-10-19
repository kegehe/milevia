export function snapshotRequestPath(instanceID: string, baseline: number, force = false) {
  const path = `/v1/instances/${encodeURIComponent(instanceID)}/snapshot`;
  return force ? path : `${path}?revision=${baseline}`;
}
