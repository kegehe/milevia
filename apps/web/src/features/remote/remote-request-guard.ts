export function isCurrentRemoteRequest(
  requestGeneration: number,
  activeGeneration: number,
  requestToken: string,
  activeToken: string,
) {
  return requestGeneration === activeGeneration && requestToken === activeToken;
}
