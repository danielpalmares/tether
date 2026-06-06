// Service worker do Tether PWA.
//
// Objetivo: tornar o client instalável (menu iniciar da TV / tela inicial do
// celular) e abrir rápido. NÃO interfere no streaming: requisições de API e
// WebSocket de signaling sempre vão à rede; o SW só cacheia o "app shell"
// estático (HTML/CSS/ícones) com estratégia network-first para o HTML (pega
// atualizações quando online) e cache-first para assets imutáveis.

const CACHE = 'tether-shell-v1';
const SHELL = [
  '/client.html',
  '/style.css',
  '/manifest.webmanifest',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/icon-maskable-512.png',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  // Nunca toca em API/signaling: streaming e descoberta precisam de dados vivos.
  if (url.pathname.startsWith('/api/') || url.pathname === '/ws') return;

  // HTML: network-first (atualiza quando online, cai pro cache offline).
  const isHTML = req.mode === 'navigate' || url.pathname.endsWith('.html');
  if (isHTML) {
    event.respondWith(
      fetch(req)
        .then((res) => {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
          return res;
        })
        .catch(() => caches.match(req).then((r) => r || caches.match('/client.html')))
    );
    return;
  }

  // Assets estáticos: cache-first com revalidação em background.
  event.respondWith(
    caches.match(req).then((cached) => {
      const network = fetch(req)
        .then((res) => {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
          return res;
        })
        .catch(() => cached);
      return cached || network;
    })
  );
});
