// service-worker.js — Phase 6(PWA/웹 푸시). 캐싱은 "앱 쉘"(index.html +
// manifest + 아이콘, 정적 파일뿐)로 최소화한다 — API 응답(공고/파이프라인
// 등 실시간성이 생명인 데이터)은 절대 캐시하지 않는다. 목적은 오프라인
// 상태에서도 화면 골격(예: 로그인 화면)만은 뜨게 하는 것 — 그 안의 실제
// 데이터는 네트워크 없이는 여전히 못 불러온다.
const CACHE_NAME = 'app-shell-v1';
const APP_SHELL_URLS = ['/', '/manifest.json', '/icons/icon-192.png', '/icons/icon-512.png'];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => cache.addAll(APP_SHELL_URLS))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((names) => Promise.all(names.filter((n) => n !== CACHE_NAME).map((n) => caches.delete(n))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.pathname.startsWith('/api/')) return; // 데이터는 절대 캐시하지 않음 — 네트워크 요청 그대로 통과

  // 페이지 이동(SPA는 해시 라우팅이라 실제 네트워크 요청은 항상 '/')은
  // 네트워크 우선 — 온라인이면 항상 최신, 오프라인일 때만 캐시된 앱
  // 쉘로 폴백해 최소한 화면 골격은 뜨게 한다.
  if (req.mode === 'navigate') {
    event.respondWith(fetch(req).catch(() => caches.match('/')));
    return;
  }

  // 그 외 정적 자산(아이콘 등)은 캐시 우선, 없으면 네트워크에서 받아와
  // 다음을 위해 캐시에 채운다.
  event.respondWith(
    caches.match(req).then((cached) => cached || fetch(req).then((res) => {
      if (res.ok) {
        const resClone = res.clone();
        caches.open(CACHE_NAME).then((cache) => cache.put(req, resClone));
      }
      return res;
    }).catch(() => cached))
  );
});

self.addEventListener('push', (event) => {
  if (!event.data) return;
  let payload;
  try {
    payload = event.data.json();
  } catch (e) {
    payload = { title: '공공사업 AI 비서', body: event.data.text() };
  }
  const title = payload.title || '공공사업 AI 비서';
  const options = {
    body: payload.body || '',
    icon: '/icons/icon-192.png',
    badge: '/icons/icon-192.png',
    data: { url: payload.url || '/' },
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || '/';
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clientList) => {
      for (const client of clientList) {
        if ('focus' in client) {
          client.navigate(url);
          return client.focus();
        }
      }
      if (self.clients.openWindow) {
        return self.clients.openWindow(url);
      }
    })
  );
});
