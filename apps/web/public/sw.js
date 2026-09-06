const CACHE = "milevia-shell-v1";
const SHELL = ["/", "/mobile", "/manifest.webmanifest", "/milevia-mark.svg"];
self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((cache) => cache.addAll(SHELL)).then(() => self.skipWaiting()));
});
self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});
self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET" || new URL(request.url).origin !== self.location.origin) return;
  const path = new URL(request.url).pathname;
  if (path.startsWith("/api/") || path.startsWith("/v1/") || path.startsWith("/ws/")) return;
  event.respondWith(fetch(request).then((response) => {
    const copy = response.clone();
    caches.open(CACHE).then((cache) => cache.put(request, copy));
    return response;
  }).catch(() => caches.match(request).then((cached) => cached || caches.match("/"))));
});
