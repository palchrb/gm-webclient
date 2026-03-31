// Service Worker for Garmin Messenger Web Push notifications

self.addEventListener('push', function(event) {
    if (!event.data) return;

    var data;
    try {
        data = event.data.json();
    } catch (e) {
        data = { body: event.data.text() };
    }

    var title = data.title || 'Garmin Messenger';
    var options = {
        body: data.body || 'New message',
        tag: data.conversationId || 'default',
        data: {
            conversationId: data.conversationId,
            url: '/'
        }
    };

    event.waitUntil(
        self.registration.showNotification(title, options)
    );
});

self.addEventListener('notificationclick', function(event) {
    event.notification.close();

    var url = event.notification.data && event.notification.data.url ? event.notification.data.url : '/';

    event.waitUntil(
        clients.matchAll({ type: 'window', includeUncontrolled: true })
            .then(function(clientList) {
                for (var i = 0; i < clientList.length; i++) {
                    var client = clientList[i];
                    if (client.url.indexOf(self.location.origin) !== -1 && 'focus' in client) {
                        return client.focus();
                    }
                }
                return clients.openWindow(url);
            })
    );
});
