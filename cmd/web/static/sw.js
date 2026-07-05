// Minimal PWA service worker: caches only the static app shell so the app
// installs cleanly and its icon/name show up correctly on a phone's home
// screen. Everything else - /api/*, /media/*, /login, /setup - is dynamic
// or sensitive and is left to go straight to the network, uncached.
const CACHE_NAME = 'streamrec-shell-v1';
const SHELL_ASSETS = [
  '/',
  '/app.css',
  '/app.js',
  '/manifest.json',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => cache.addAll(SHELL_ASSETS)).catch(() => {})
  );
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k)))
    )
  );
  self.clients.claim();
});

function isShellRequest(url) {
  if (url.origin !== self.location.origin) return false;
  if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/media/')) return false;
  if (url.pathname === '/login' || url.pathname === '/setup') return false;
  return SHELL_ASSETS.includes(url.pathname) || url.pathname === '/';
}

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (event.request.method !== 'GET' || !isShellRequest(url)) return;

  // Network-first: always prefer a live copy of the shell, but fall back to
  // the cached version if the network is unreachable (e.g. briefly offline
  // between wifi/cellular handoff on a phone).
  event.respondWith(
    fetch(event.request)
      .then((res) => {
        const copy = res.clone();
        caches.open(CACHE_NAME).then((cache) => cache.put(event.request, copy));
        return res;
      })
      .catch(() => caches.match(event.request))
  );
});
