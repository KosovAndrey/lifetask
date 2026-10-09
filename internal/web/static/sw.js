// Сервис-воркер LifeTask: оболочка и последние ответы API доступны без сети.
// Запись офлайн копит сама страница (IndexedDB) — здесь только чтение.
const SHELL = 'lp-shell-v4';
const API = 'lp-api-v1';
const SHELL_FILES = ['/', '/index.html', '/app.js', '/queue.mjs', '/shell.mjs', '/style.css', '/manifest.webmanifest', '/icon.svg', '/icon-180.png', '/icon-512.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(SHELL).then((c) => c.addAll(SHELL_FILES)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys()
    .then((keys) => Promise.all(keys.filter((k) => k !== SHELL && k !== API).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

// Сеть в приоритете (данные и обновления свежие), кеш — запасной путь.
self.addEventListener('fetch', (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== 'GET' || url.origin !== location.origin || url.pathname === '/login') return;

  // Вложения не кешируем здесь: они тяжёлые, а браузер и так держит их по Cache-Control.
  if (url.pathname.startsWith('/api/files/')) return;

  if (url.pathname.startsWith('/api/')) {
    e.respondWith(fetch(req).then((res) => {
      if (res.ok) {
        const copy = res.clone();
        caches.open(API).then((c) => c.put(req, copy));
      }
      return res;
    }).catch(async () => {
      const hit = await caches.match(req, { cacheName: API });
      if (!hit) {
        return new Response(JSON.stringify({ error: 'нет сети, а эти данные ещё не открывались' }),
          { status: 503, headers: { 'Content-Type': 'application/json', 'X-LP-Offline': '1' } });
      }
      const headers = new Headers(hit.headers);
      headers.set('X-LP-Offline', '1');
      return new Response(await hit.blob(), { status: 200, headers });
    }));
    return;
  }

  e.respondWith(fetch(req).then((res) => {
    if (res.ok) {
      const copy = res.clone();
      caches.open(SHELL).then((c) => c.put(req, copy));
    }
    return res;
  }).catch(() => caches.match(req, { cacheName: SHELL }).then((hit) => hit || caches.match('/', { cacheName: SHELL }))));
});
