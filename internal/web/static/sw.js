// Service Worker for Garmin Messenger Web — Push notifications + PWA offline shell

var SHELL_CACHE = 'gm-shell-v1';
var SHELL_URLS = ['/', '/index.html', '/app.js', '/style.css', '/manifest.json', '/icon.svg'];

self.addEventListener('install', function(event) {
    event.waitUntil(
        caches.open(SHELL_CACHE)
            .then(function(cache) { return cache.addAll(SHELL_URLS); })
            .catch(function() { /* offline install; shell fills in on first online load */ })
            .then(function() { return self.skipWaiting(); })
    );
});

self.addEventListener('activate', function(event) {
    event.waitUntil(
        caches.keys()
            .then(function(keys) {
                return Promise.all(keys.filter(function(k) { return k !== SHELL_CACHE; })
                    .map(function(k) { return caches.delete(k); }));
            })
            .then(function() { return clients.claim(); })
    );
});

// App shell: network-first so deploys propagate immediately, cache fallback
// so the app still opens offline and can show locally cached conversations.
// API calls and media are never cached here — the browser HTTP cache and
// the app's own local cache handle those.
self.addEventListener('fetch', function(event) {
    var req = event.request;
    if (req.method !== 'GET') return;
    var url = new URL(req.url);
    if (url.origin !== self.location.origin) return;
    if (url.pathname.indexOf('/api/') === 0) return;

    event.respondWith(
        fetch(req)
            .then(function(resp) {
                if (resp && resp.ok) {
                    var copy = resp.clone();
                    caches.open(SHELL_CACHE).then(function(cache) { cache.put(req, copy); });
                }
                return resp;
            })
            .catch(function() {
                return caches.match(req).then(function(cached) {
                    if (cached) return cached;
                    if (req.mode === 'navigate') return caches.match('/');
                    return Response.error();
                });
            })
    );
});

// ─── Push ────────────────────────────────────────────────────────────────────
// The payload is content-free: { conversationId, messageId }. We fetch sender
// and preview from our own server over the session cookie so message text
// never passes through the browser vendor's push service. If the fetch fails
// (logged out, offline), a generic notification is shown instead.

function isReaction(body) {
    return typeof body === 'string' && body.charCodeAt(0) === 0x200b;
}

function previewFor(msg) {
    if (!msg) return 'New message';
    var body = msg.messageBody || '';
    if (body && !isReaction(body)) return body;
    if (msg.mediaType === 'ImageAvif') return '📷 Photo';
    if (msg.mediaType === 'AudioOgg') return '🎤 Voice message';
    if (msg.userLocation || msg.location) return '📍 Location';
    return 'New message';
}

// "Garmin: +4740847119" when the sender is a phone number; inReach devices
// report a UUID, which is not useful in a title.
function titleFor(msg) {
    var from = (msg && msg.from) || '';
    if (/^\+[0-9]{7,15}$/.test(from)) return 'Garmin: ' + from;
    return 'Garmin Messenger';
}

function fetchNotification(conversationId, messageId) {
    var fallback = { title: 'Garmin Messenger', body: 'New message' };
    if (!conversationId) return Promise.resolve(fallback);
    return fetch('/api/conversations/' + encodeURIComponent(conversationId) + '?limit=5', {
        credentials: 'same-origin',
        cache: 'no-store'
    })
        .then(function(r) { return r.ok ? r.json() : null; })
        .then(function(data) {
            var msgs = (data && data.messages) || [];
            var hit = null;
            for (var i = 0; i < msgs.length; i++) {
                if (msgs[i].messageId === messageId) { hit = msgs[i]; break; }
            }
            if (!hit && msgs.length) {
                msgs.sort(function(a, b) {
                    return new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0);
                });
                hit = msgs[msgs.length - 1];
            }
            if (!hit) return fallback;
            return { title: titleFor(hit), body: previewFor(hit) };
        })
        .catch(function() { return fallback; });
}

self.addEventListener('push', function(event) {
    var data = {};
    if (event.data) {
        try { data = event.data.json(); } catch (e) { data = {}; }
    }
    var conversationId = data.conversationId || '';

    event.waitUntil(
        fetchNotification(conversationId, data.messageId).then(function(n) {
            return self.registration.showNotification(n.title, {
                body: n.body,
                icon: '/icon.svg',
                badge: '/icon.svg',
                tag: conversationId || 'default',
                renotify: true,
                data: {
                    conversationId: conversationId,
                    url: conversationId ? '/#conversation/' + conversationId : '/'
                }
            });
        })
    );
});

self.addEventListener('notificationclick', function(event) {
    event.notification.close();
    var d = event.notification.data || {};
    var conversationId = d.conversationId || '';
    var url = d.url || '/';

    event.waitUntil(
        clients.matchAll({ type: 'window', includeUncontrolled: true })
            .then(function(clientList) {
                for (var i = 0; i < clientList.length; i++) {
                    var client = clientList[i];
                    if (client.url.indexOf(self.location.origin) === 0 && 'focus' in client) {
                        if (conversationId) {
                            client.postMessage({ type: 'open-conversation', conversationId: conversationId });
                        }
                        return client.focus();
                    }
                }
                return clients.openWindow(url);
            })
    );
});
