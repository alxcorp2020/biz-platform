// service-worker.js — Phase 6(PWA/웹 푸시). 캐싱은 "앱 쉘"(index.html +
// manifest + 아이콘, 정적 파일뿐)로 최소화한다 — API 응답(공고/파이프라인
// 등 실시간성이 생명인 데이터)은 절대 캐시하지 않는다. 목적은 오프라인
// 상태에서도 화면 골격(예: 로그인 화면)만은 뜨게 하는 것 — 그 안의 실제
// 데이터는 네트워크 없이는 여전히 못 불러온다.
const CACHE_NAME = 'app-shell-v2'; // v2: chrome-extension:// 스킴 요청을 캐시에 넣으려다 나던 예외 수정(2026-08-04)
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
  // 서비스워커의 fetch 이벤트는 이 페이지 안에서 일어나는 모든 네트워크
  // 요청을 가로챈다 — 페이지 자체의 요청뿐 아니라, 사용자가 설치한 다른
  // 브라우저 확장 프로그램이 이 탭 컨텍스트에서 만드는 chrome-extension://
  // 스킴 요청까지 포함된다. Cache API는 http(s) 스킴만 지원해서
  // cache.put()에 그런 요청을 넘기면 예외가 난다 — 이 서비스워커와 무관한
  // 요청이니 아예 손대지 않고 그냥 통과시킨다.
  if (url.protocol !== 'http:' && url.protocol !== 'https:') return;
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

// fetchBrandName — 서버가 payload에 title을 안 실어 보낸 경우에만 쓰는
// 최후의 대비책(서버 발신 로직은 항상 명시적 title을 채워 보내므로 정상
// 경로에서는 거의 안 탄다). 서비스워커는 페이지의 currentBrandName 전역을
// 못 보므로 공개 API(/api/company-info)를 직접 불러 브랜드명이 바뀌어도
// 여기 값이 낡지 않게 한다. 네트워크 자체가 실패하면(오프라인 등)
// 진짜 최후의 하드코딩 기본값으로 떨어진다.
async function fetchBrandName() {
  try {
    const res = await fetch('/api/company-info');
    const data = await res.json();
    return data.brandName || '공공사업 AI 비서';
  } catch (e) {
    return '공공사업 AI 비서';
  }
}

self.addEventListener('push', (event) => {
  if (!event.data) return;
  event.waitUntil((async () => {
    let payload;
    try {
      payload = event.data.json();
    } catch (e) {
      payload = { title: null, body: event.data.text() };
    }
    const title = payload.title || await fetchBrandName();
    const options = {
      body: payload.body || '',
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      data: { url: payload.url || '/' },
    };
    return self.registration.showNotification(title, options);
  })());
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
