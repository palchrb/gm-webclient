// ─── State ───────────────────────────────────────────────────────────────────
const state = {
    loggedIn: false,
    phone: null,
    userId: null,
    conversations: [],
    currentConversationId: null,
    messages: [],
    members: {},         // convId -> UserInfoModel[]
    memberNames: {},     // userId/phone -> display name (global cache)
    eventSource: null,
};

// ─── Local Cache ─────────────────────────────────────────────────────────────
// Cache conversations and members in localStorage to avoid hammering the
// Garmin API on every page load. Data is refreshed in the background.
const cache = {
    set(key, value) {
        try {
            localStorage.setItem('gm_' + key, JSON.stringify({ t: Date.now(), v: value }));
        } catch (e) { /* quota exceeded or private mode */ }
    },
    get(key, maxAgeMs) {
        try {
            const raw = localStorage.getItem('gm_' + key);
            if (!raw) return null;
            const { t, v } = JSON.parse(raw);
            if (maxAgeMs && Date.now() - t > maxAgeMs) return null;
            return v;
        } catch (e) { return null; }
    },
    remove(key) {
        try { localStorage.removeItem('gm_' + key); } catch (e) {}
    },
    clear() {
        try {
            const keys = [];
            for (let i = 0; i < localStorage.length; i++) {
                const k = localStorage.key(i);
                if (k && k.startsWith('gm_')) keys.push(k);
            }
            keys.forEach(k => localStorage.removeItem(k));
        } catch (e) {}
    },
};

// ─── Init ────────────────────────────────────────────────────────────────────
async function init() {
    try {
        const resp = await api('/api/auth/status');
        if (resp.loggedIn) {
            state.loggedIn = true;
            state.phone = resp.phone;
            state.userId = (resp.userId || '').toLowerCase();
            showChatView();
            await loadConversations();
            connectSSE();
            setupPushNotifications();
            setupNtfyButton();
            setupClipboardPaste();

            // Open conversation from URL hash (e.g. #conversation/<id> from ntfy)
            var hash = window.location.hash;
            if (hash.startsWith('#conversation/')) {
                var convId = hash.replace('#conversation/', '');
                if (convId) selectConversation(convId);
                history.replaceState(null, '', window.location.pathname);
            }
            return;
        }
    } catch (e) {
        // Not logged in
    }
    // Show login view only after confirming not logged in
    document.getElementById('login-view').classList.remove('hidden');
}

// Paste images from clipboard directly into the composer
function setupClipboardPaste() {
    document.addEventListener('paste', (e) => {
        if (!state.currentConversationId) return;
        const items = e.clipboardData && e.clipboardData.items;
        if (!items) return;
        for (const item of items) {
            if (item.type.startsWith('image/')) {
                e.preventDefault();
                const file = item.getAsFile();
                if (file) stageMediaPreview(file);
                return;
            }
        }
    });
}

// ─── Auth ────────────────────────────────────────────────────────────────────

function hideAllLoginSteps() {
    for (const id of ['phone-step', 'otp-step', 'reauth-step', 'passkey-setup-step', 'passkey-login-step']) {
        document.getElementById(id).classList.add('hidden');
    }
}

function enterChat(resp) {
    state.loggedIn = true;
    state.phone = resp.phone;
    state.userId = (resp.userId || '').toLowerCase();
    showChatView();
    loadConversations();
    connectSSE();
    setupPushNotifications();
    setupNtfyButton();
    setupClipboardPaste();
}

async function startLogin() {
    const phone = document.getElementById('phone-input').value.trim();
    if (!phone) return;

    setLoading(true);
    hideError();

    try {
        const resp = await api('/api/auth/request-otp', { method: 'POST', body: { phone } });
        hideAllLoginSteps();

        if (resp.needPasskey) {
            // Show passkey step with OTP fallback option
            document.getElementById('passkey-login-step').classList.remove('hidden');
        } else {
            // Normal OTP flow (first login or no passkey)
            document.getElementById('otp-step').classList.remove('hidden');
            document.getElementById('otp-phone').textContent = phone;
            document.getElementById('otp-input').focus();
        }
    } catch (e) {
        showError(e.message || 'Could not send code');
    } finally {
        setLoading(false);
    }
}

// Called from the passkey-login-step "Use passkey" button
async function doPasskeyLogin() {
    const phone = document.getElementById('phone-input').value.trim();
    if (!phone) return;
    await passkeyLogin(phone);
}

// Called from the passkey-login-step "Use OTP instead" link
async function requestOTPInstead() {
    const phone = document.getElementById('phone-input').value.trim();
    if (!phone) return;

    setLoading(true);
    hideError();

    try {
        await api('/api/auth/request-otp', { method: 'POST', body: { phone, forceOTP: true } });
        hideAllLoginSteps();
        document.getElementById('otp-step').classList.remove('hidden');
        document.getElementById('otp-phone').textContent = phone;
        document.getElementById('otp-input').focus();
    } catch (e) {
        showError(e.message || 'Could not send OTP');
    } finally {
        setLoading(false);
    }
}

async function confirmOTP() {
    const phone = document.getElementById('phone-input').value.trim();
    const code = document.getElementById('otp-input').value.trim();
    if (!phone || !code) return;

    setLoading(true);
    hideError();

    try {
        const resp = await api('/api/auth/confirm-otp', { method: 'POST', body: { phone, code } });

        if (resp.needPasskeySetup && window.PublicKeyCredential) {
            // Must register passkey before entering chat
            hideAllLoginSteps();
            document.getElementById('passkey-setup-step').classList.remove('hidden');
            // Store login state temporarily so we can enter chat after passkey
            state._pendingLogin = resp;
        } else {
            enterChat(resp);
        }
    } catch (e) {
        showError(e.message || 'Invalid code');
    } finally {
        setLoading(false);
    }
}

function skipPasskeySetup() {
    if (state._pendingLogin) {
        enterChat(state._pendingLogin);
        delete state._pendingLogin;
    }
}

// ─── Passkey (WebAuthn) ─────────────────────────────────────────────────────

function bufferToBase64url(buf) {
    const bytes = new Uint8Array(buf);
    let str = '';
    for (const b of bytes) str += String.fromCharCode(b);
    return btoa(str).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function base64urlToBuffer(b64) {
    const padded = b64.replace(/-/g, '+').replace(/_/g, '/');
    const binary = atob(padded);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    return bytes.buffer;
}

async function registerPasskey() {
    setLoading(true);
    hideError();

    try {
        const options = await api('/api/passkey/register/begin', { method: 'POST' });

        options.publicKey.challenge = base64urlToBuffer(options.publicKey.challenge);
        options.publicKey.user.id = base64urlToBuffer(options.publicKey.user.id);
        if (options.publicKey.excludeCredentials) {
            for (const c of options.publicKey.excludeCredentials) {
                c.id = base64urlToBuffer(c.id);
            }
        }

        const credential = await navigator.credentials.create(options);

        const body = JSON.stringify({
            id: credential.id,
            rawId: bufferToBase64url(credential.rawId),
            type: credential.type,
            response: {
                attestationObject: bufferToBase64url(credential.response.attestationObject),
                clientDataJSON: bufferToBase64url(credential.response.clientDataJSON),
            },
        });

        const resp = await fetch('/api/passkey/register/finish', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body,
        });
        if (!resp.ok) {
            const err = await resp.json().catch(() => ({}));
            throw new Error(err.error || 'Passkey registration failed');
        }

        console.log('Passkey registered');

        // Enter chat with the pending login state (mandatory setup flow)
        if (state._pendingLogin) {
            enterChat(state._pendingLogin);
            delete state._pendingLogin;
        }
    } catch (e) {
        showError(e.message || 'Passkey registration failed. Try again.');
    } finally {
        setLoading(false);
    }
}

async function passkeyLogin(phone) {
    try {
        setLoading(true);
        const options = await api('/api/passkey/login/begin', { method: 'POST', body: { phone } });

        options.publicKey.challenge = base64urlToBuffer(options.publicKey.challenge);
        if (options.publicKey.allowCredentials) {
            for (const c of options.publicKey.allowCredentials) {
                c.id = base64urlToBuffer(c.id);
            }
        }

        const assertion = await navigator.credentials.get(options);

        const body = JSON.stringify({
            id: assertion.id,
            rawId: bufferToBase64url(assertion.rawId),
            type: assertion.type,
            response: {
                authenticatorData: bufferToBase64url(assertion.response.authenticatorData),
                clientDataJSON: bufferToBase64url(assertion.response.clientDataJSON),
                signature: bufferToBase64url(assertion.response.signature),
                userHandle: assertion.response.userHandle ? bufferToBase64url(assertion.response.userHandle) : '',
            },
        });

        const resp = await fetch('/api/passkey/login/finish?phone=' + encodeURIComponent(phone), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body,
        });
        if (!resp.ok) {
            const err = await resp.json().catch(() => ({}));
            throw new Error(err.error || 'Passkey login failed');
        }

        const data = await resp.json();

        if (data.needReauth) {
            // Passkey verified but Garmin tokens expired — need OTP to reconnect
            hideAllLoginSteps();
            document.getElementById('reauth-step').classList.remove('hidden');
            document.getElementById('reauth-phone').textContent = phone;
            return;
        }

        enterChat(data);
    } catch (e) {
        // All passkey failures (cancelled, not found, verification error) → OTP fallback
        hideAllLoginSteps();
        try {
            await api('/api/auth/request-otp', { method: 'POST', body: { phone, forceOTP: true } });
            const reason = e.name === 'NotAllowedError'
                ? 'Passkey not available — enter the code sent to your phone.'
                : 'Passkey failed — enter the code sent to your phone instead.';
            showError(reason);
            document.getElementById('otp-step').classList.remove('hidden');
            document.getElementById('otp-phone').textContent = phone;
            document.getElementById('otp-input').focus();
        } catch (e2) {
            document.getElementById('phone-step').classList.remove('hidden');
            showError(e2.message || 'Could not send OTP. Try again.');
        }
    } finally {
        setLoading(false);
    }
}

async function requestReauthOTP() {
    const phone = document.getElementById('phone-input').value.trim();
    if (!phone) return;

    setLoading(true);
    hideError();

    try {
        await api('/api/auth/request-reauth-otp', { method: 'POST', body: { phone } });
        hideAllLoginSteps();
        document.getElementById('otp-step').classList.remove('hidden');
        document.getElementById('otp-phone').textContent = phone;
        document.getElementById('otp-input').focus();
    } catch (e) {
        showError(e.message || 'Could not send code');
    } finally {
        setLoading(false);
    }
}

// ─── Account Menu ────────────────────────────────────────────────────────────

function showAccountMenu() {
    document.getElementById('logout-all-confirm').classList.add('hidden');
    document.getElementById('clear-passkeys-check').checked = false;
    document.getElementById('account-modal').classList.remove('hidden');
}

function hideAccountMenu() {
    document.getElementById('account-modal').classList.add('hidden');
}

function showNotificationMenu() {
    document.getElementById('notification-modal').classList.remove('hidden');
}

function hideNotificationMenu() {
    document.getElementById('notification-modal').classList.add('hidden');
}

function showLogoutAllConfirm() {
    document.getElementById('logout-all-confirm').classList.remove('hidden');
}

async function addPasskey() {
    hideAccountMenu();
    if (!window.PublicKeyCredential) {
        alert('Passkeys are not supported in this browser.');
        return;
    }
    try {
        await registerPasskey();
        alert('Passkey registered!');
    } catch (e) {
        // registerPasskey already handles errors
    }
}

function doLogoutCleanup() {
    if (state.eventSource) state.eventSource.close();
    cache.clear();
    state.loggedIn = false;
    state.phone = null;
    state.conversations = [];
    state.currentConversationId = null;
    state.messages = [];
    location.reload();
}

async function logoutThis() {
    hideAccountMenu();
    try { await api('/api/auth/logout', { method: 'POST' }); } catch (e) { /* ignore */ }
    doLogoutCleanup();
}

async function logoutAll() {
    const clearPasskeys = document.getElementById('clear-passkeys-check').checked;
    hideAccountMenu();
    try {
        await api('/api/auth/logout-all', {
            method: 'POST',
            body: { clearPasskeys },
        });
    } catch (e) { /* ignore */ }
    doLogoutCleanup();
}

// ─── Conversations ───────────────────────────────────────────────────────────
let lastConversationCursor = null; // pagination cursor from API
let hasMoreConversations = false;

async function loadConversations() {
    const cached = cache.get('conversations');
    const cachedMembers = cache.get('members');
    const cachedNames = cache.get('memberNames');

    if (cached) {
        state.conversations = cached;
        if (cachedMembers) state.members = cachedMembers;
        if (cachedNames) state.memberNames = cachedNames;
        renderConversations();
    }

    try {
        const resp = await api('/api/conversations');
        state.conversations = (resp.conversations || []).sort(
            (a, b) => new Date(b.updatedDate) - new Date(a.updatedDate)
        );
        lastConversationCursor = resp.lastConversationId || null;
        hasMoreConversations = !!lastConversationCursor;
        cache.set('conversations', state.conversations);
        computeOfflineUnread(); // badge conversations updated while we were offline
        renderConversations();
        for (const conv of state.conversations) {
            loadMembers(conv.conversationId);
        }
    } catch (e) {
        console.error('Failed to load conversations:', e);
    }
}

async function loadMoreConversations() {
    if (!lastConversationCursor) return;
    try {
        const resp = await api(`/api/conversations?after=${lastConversationCursor}`);
        const more = resp.conversations || [];

        // Deduplicate and count actually new conversations
        const existing = new Set(state.conversations.map(c => c.conversationId));
        let added = 0;
        for (const conv of more) {
            if (!existing.has(conv.conversationId)) {
                state.conversations.push(conv);
                added++;
            }
        }

        // If no new conversations were added, we've loaded everything
        if (added === 0) {
            hasMoreConversations = false;
        } else {
            state.conversations.sort((a, b) => new Date(b.updatedDate) - new Date(a.updatedDate));
            lastConversationCursor = resp.lastConversationId || null;
            hasMoreConversations = !!lastConversationCursor && added > 0;
            for (const conv of more) {
                loadMembers(conv.conversationId);
            }
        }

        cache.set('conversations', state.conversations);
        renderConversations();
    } catch (e) {
        console.error('Failed to load more conversations:', e);
    }
}

async function loadMembers(convId) {
    if (state.members[convId]) {
        // Members cached — but ensure memberNames are populated (cache may be stale)
        for (const m of state.members[convId]) {
            const name = m.friendlyName || m.address || '';
            if (!name) continue;
            if (m.userIdentifier) state.memberNames[m.userIdentifier.toLowerCase()] = name;
            if (m.address) state.memberNames[m.address] = name;
        }
        return;
    }
    try {
        const members = await api(`/api/conversations/${convId}/members`);
        state.members[convId] = members;
        for (const m of members) {
            const name = m.friendlyName || m.address || '';
            if (!name) continue;
            // Store by both UUID (lowercased) and phone number for reliable lookup
            if (m.userIdentifier) {
                state.memberNames[m.userIdentifier.toLowerCase()] = name;
            }
            if (m.address) {
                state.memberNames[m.address] = name;
            }
        }
        // Persist to cache
        cache.set('members', state.members);
        cache.set('memberNames', state.memberNames);
        renderConversations();
        if (state.currentConversationId === convId) {
            renderMessages();
        }
    } catch (e) {
        console.error('Failed to load members:', e);
    }
}

async function selectConversation(convId) {
    state.currentConversationId = convId;
    delete unreadCounts[convId]; // Clear unread badge
    setLastSeen(convId);
    renderConversations(); // Update badge display

    document.getElementById('no-conversation').classList.add('hidden');
    document.getElementById('conversation-view').classList.remove('hidden');

    // Close sidebar on mobile
    document.getElementById('sidebar').classList.remove('open');

    // Show cached messages immediately — no network wait.
    const cachedMsgs = cache.get('msgs_' + convId);
    const container = document.getElementById('messages');
    if (cachedMsgs) {
        state.messages = cachedMsgs;
        renderMessages();
        container.scrollTop = container.scrollHeight;
    }

    // Refresh from API using lightweight diff — only appends new messages,
    // no innerHTML nuke, no scroll jump.
    try {
        if (cachedMsgs) {
            // We already showed cached, just catch up with any new messages
            await catchUpConversation(convId);
        } else {
            // First visit to this conversation — full load required
            const resp = await api(`/api/conversations/${convId}?limit=200`);
            state.messages = (resp.messages || []).sort(
                (a, b) => new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0)
            );
            cache.set('msgs_' + convId, state.messages);
            renderMessages();
            container.scrollTop = container.scrollHeight;

            if (state.messages.length > 0) {
                const lastMsg = state.messages[state.messages.length - 1];
                markAsRead(convId, lastMsg.messageId);
            }
        }
    } catch (e) {
        console.error('Failed to load messages:', e);
    }

    await loadMembers(convId);
    renderConversations();
    updateConversationTitle(convId);
}

function deselectConversation() {
    state.currentConversationId = null;
    document.getElementById('no-conversation').classList.remove('hidden');
    document.getElementById('conversation-view').classList.add('hidden');
    renderConversations();
}

async function leaveConversation(convId) {
    if (!confirm('Leave this conversation? It will be removed from your list.')) return;

    try {
        await api(`/api/conversations/${convId}/leave`, { method: 'POST' });
        // Remove from local state
        state.conversations = state.conversations.filter(c => c.conversationId !== convId);
        delete state.members[convId];
        if (state.currentConversationId === convId) {
            deselectConversation();
        }
        renderConversations();
    } catch (e) {
        console.error('Failed to leave conversation:', e);
        alert('Failed to leave conversation: ' + e.message);
    }
}

// ─── Messages ────────────────────────────────────────────────────────────────
async function sendMessage() {
    const input = document.getElementById('message-input');
    const body = input.value.trim();

    // If there's a staged media file, send it (with optional caption)
    if (stagedMediaFile) {
        const file = stagedMediaFile;
        cancelMediaPreview();
        input.value = '';
        sendMediaFile(file, body);
        return;
    }

    if (!body || !state.currentConversationId) return;

    const to = getRecipientPhones(state.currentConversationId);
    if (to.length === 0) {
        console.error('No recipient phone numbers found — members may not be loaded yet');
        return;
    }

    const convId = state.currentConversationId;
    input.value = '';

    // Show message immediately BEFORE the API call
    const tempId = 'sending-' + Date.now();
    addOptimisticMessage(convId, tempId, body, 'sending');

    try {
        const result = await api('/api/messages/send', {
            method: 'POST',
            body: { conversationId: convId, to, body }
        });
        replaceOptimisticMessage(convId, tempId, result.messageId, 'sent');
    } catch (e) {
        console.error('Failed to send message:', e);
        markOptimisticFailed(tempId, e.message || 'Send failed');
    }
}

// Get recipient phone numbers (addresses) for a conversation, excluding ourselves.
function getRecipientPhones(convId) {
    const members = state.members[convId] || [];
    const phones = [];
    for (const m of members) {
        const addr = m.address || '';
        const id = m.userIdentifier || '';
        // Skip ourselves
        if (id === state.userId || addr === state.phone) continue;
        if (addr) phones.push(addr);
    }
    // Fallback: if members aren't loaded yet, use memberIds (UUIDs) from conversation
    if (phones.length === 0) {
        const conv = state.conversations.find(c => c.conversationId === convId);
        if (conv) {
            return conv.memberIds.filter(id => id !== state.userId);
        }
    }
    return phones;
}

// Add an optimistic message to the current conversation view immediately.
// sendState: 'sending' | 'sent' | 'failed'
function addOptimisticMessage(convId, messageId, body, sendState) {
    if (state.currentConversationId !== convId) return;
    if (state.messages.find(m => m.messageId === messageId)) return;
    const msg = {
        messageId: messageId,
        conversationId: convId,
        from: state.userId,
        messageBody: body,
        sentAt: new Date().toISOString(),
        status: [],
        _sendState: sendState || 'sent',
    };
    state.messages.push(msg);
    appendMessageToDOM(msg);
    scrollToBottom(true);
}

// Replace a temporary optimistic message with the real server ID.
// Updates DOM in-place to avoid full re-render scroll jump.
function replaceOptimisticMessage(convId, tempId, realId, sendState) {
    const msg = state.messages.find(m => m.messageId === tempId);
    if (msg) {
        msg.messageId = realId;
        msg._sendState = sendState || 'sent';
        // Update the DOM element in-place instead of full re-render
        const el = document.querySelector(`[data-msgid="${tempId}"]`);
        if (el) {
            el.dataset.msgid = realId;
            el.classList.remove('message-sending');
            const statusEl = el.querySelector('.message-status');
            if (statusEl) statusEl.innerHTML = '&#10003;'; // ✓
        }
    }
}

// Mark an optimistic message as failed.
function markOptimisticFailed(tempId, errorMsg) {
    const msg = state.messages.find(m => m.messageId === tempId);
    if (msg) {
        msg._sendState = 'failed';
        msg._errorMsg = errorMsg;
        updateMessageFailed(tempId);
    }
}

// Reload the current conversation messages from the server.
// Preserves optimistic (sending/sent) messages that aren't in the server response yet.
async function reloadCurrentConversation(delayMs) {
    if (delayMs) await new Promise(r => setTimeout(r, delayMs));
    const convId = state.currentConversationId;
    if (!convId) return;
    try {
        const resp = await api(`/api/conversations/${convId}?limit=200`);
        const serverMsgs = (resp.messages || []).sort(
            (a, b) => new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0)
        );

        const serverIds = new Set(serverMsgs.map(m => m.messageId));
        const optimistic = state.messages.filter(m => {
            if (!m._sendState) return false;
            if (m._sendState === 'failed') return true;
            if (m.messageId.startsWith('sending-')) return false;
            return !serverIds.has(m.messageId);
        });

        const newMessages = [...serverMsgs, ...optimistic];

        // Only re-render if message list actually changed (new/removed messages
        // or new media appeared on existing messages)
        const msgKey = m => m.messageId + (m.mediaId || '') + (m._sendState || '');
        const oldKey = state.messages.map(msgKey).join(',');
        const newKey = newMessages.map(msgKey).join(',');
        if (oldKey === newKey) return;

        state.messages = newMessages;
        cache.set('msgs_' + convId, serverMsgs);
        renderMessages();
        scrollToBottom();
    } catch (e) {
        console.error('Failed to reload conversation:', e);
    }
}

// Lightweight catch-up: fetch only messages newer than what we have cached.
async function catchUpConversation(convId) {
    if (!convId) return;
    try {
        // Find the newest real (non-optimistic) message ID to use as cursor
        var newerParam = '';
        for (var i = state.messages.length - 1; i >= 0; i--) {
            if (!state.messages[i].messageId.startsWith('sending-')) {
                newerParam = '&newerThanId=' + state.messages[i].messageId;
                break;
            }
        }

        const resp = await api('/api/conversations/' + convId + '?limit=200' + newerParam);
        if (state.currentConversationId !== convId) return;
        const newMsgs = (resp.messages || []).sort(
            (a, b) => new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0)
        );

        if (newMsgs.length === 0) return;

        const existingIds = new Set(state.messages.map(m => m.messageId));
        let added = false;
        for (const msg of newMsgs) {
            if (existingIds.has(msg.messageId)) {
                // Update existing message (e.g. media arrived)
                var existing = state.messages.find(m => m.messageId === msg.messageId);
                if (existing) {
                    var hadMedia = existing.mediaId && existing.mediaId !== '00000000-0000-0000-0000-000000000000';
                    var hasMedia = msg.mediaId && msg.mediaId !== '00000000-0000-0000-0000-000000000000';
                    if (hasMedia && !hadMedia) {
                        Object.assign(existing, msg);
                        delete existing._sendState;
                        delete existing._errorMsg;
                        rebuildMessageDOM(msg.messageId);
                    } else if (existing._sendState) {
                        delete existing._sendState;
                        delete existing._errorMsg;
                        var el = document.querySelector('[data-msgid="' + msg.messageId + '"]');
                        if (el) el.classList.remove('message-sending');
                    }
                }
                continue;
            }
            state.messages.push(msg);
            if (isReactionMessage(msg)) {
                // New reaction arrived — find target and update its badge
                var r = extractReaction(msg);
                if (r) {
                    var target = findReactionTarget(r, msg);
                    if (target) updateMessageReactions(target.messageId);
                }
            } else {
                appendMessageToDOM(msg);
                added = true;
            }
        }

        // Sort to fix any ordering issues from live appends
        state.messages.sort(function(a, b) {
            return new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0);
        });

        cache.set('msgs_' + convId, state.messages);
        if (added) scrollToBottom();

        // Mark last message as read
        if (state.messages.length > 0) {
            const lastMsg = state.messages[state.messages.length - 1];
            markAsRead(convId, lastMsg.messageId);
        }
    } catch (e) {
        console.error('Failed to catch up conversation:', e);
    }
}

// Targeted fetch for a single message that needs media (image/audio).
// Uses newerThanId with the previous message to fetch only 1-5 messages.
async function fetchMediaForMessage(convId, msgId) {
    var prevId = '';
    for (var i = 0; i < state.messages.length; i++) {
        if (state.messages[i].messageId === msgId) break;
        if (!state.messages[i].messageId.startsWith('sending-')) {
            prevId = state.messages[i].messageId;
        }
    }
    var url = '/api/conversations/' + convId + '?limit=5';
    if (prevId) url += '&newerThanId=' + prevId;

    try {
        var resp = await api(url);
        var msgs = resp.messages || [];
        var serverMsg = msgs.find(function(m) { return m.messageId === msgId; });
        if (!serverMsg) return;
        var existing = state.messages.find(function(m) { return m.messageId === msgId; });
        if (!existing) return;
        var hadMedia = existing.mediaId && existing.mediaId !== '00000000-0000-0000-0000-000000000000';
        Object.assign(existing, serverMsg);
        delete existing._sendState;
        delete existing._errorMsg;
        delete existing._needsMedia;
        cache.set('msgs_' + convId, state.messages);
        if (serverMsg.mediaId && !hadMedia) {
            rebuildMessageDOM(msgId);
        }
    } catch (e) {
        console.error('Failed to fetch media for message:', e);
    }
}

async function loadOlderMessages() {
    const convId = state.currentConversationId;
    if (!convId || state.messages.length === 0) return;
    // Find the oldest non-optimistic message ID
    const oldest = state.messages.find(m => !m._sendState);
    if (!oldest) return;

    const container = document.getElementById('messages');
    const scrollHeightBefore = container.scrollHeight;

    try {
        const resp = await api(`/api/conversations/${convId}?limit=200&olderThanId=${oldest.messageId}`);
        const older = (resp.messages || []).sort(
            (a, b) => new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0)
        );
        if (older.length === 0) return;
        // Prepend older messages, deduplicate
        const existing = new Set(state.messages.map(m => m.messageId));
        const newMsgs = older.filter(m => !existing.has(m.messageId));
        state.messages = [...newMsgs, ...state.messages];
        cache.set('msgs_' + convId, state.messages);
        renderMessages();
        // Maintain scroll position (don't jump)
        requestAnimationFrame(() => {
            container.scrollTop = container.scrollHeight - scrollHeightBefore;
        });
    } catch (e) {
        console.error('Failed to load older messages:', e);
    }
}

function handleMessageKeydown(event) {
    if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        sendMessage();
    }
}

async function markAsRead(convId, msgId) {
    try {
        await api(`/api/messages/${convId}/${msgId}/read`, { method: 'POST' });
    } catch (e) {
        // Silent fail for read receipts
    }
}

// ─── New Chat ────────────────────────────────────────────────────────────────
function showNewChatDialog() {
    document.getElementById('new-chat-dialog').classList.remove('hidden');
    document.getElementById('new-chat-phone').focus();
    document.getElementById('sidebar').classList.remove('open');
}

function hideNewChatDialog() {
    document.getElementById('new-chat-dialog').classList.add('hidden');
    document.getElementById('new-chat-phone').value = '';
    document.getElementById('new-chat-message').value = '';
    document.getElementById('new-chat-error').classList.add('hidden');
}

async function startNewChat() {
    const phone = document.getElementById('new-chat-phone').value.trim();
    const body = document.getElementById('new-chat-message').value.trim();

    if (!phone || !body) {
        document.getElementById('new-chat-error').textContent = 'Please enter a phone number and message';
        document.getElementById('new-chat-error').classList.remove('hidden');
        return;
    }

    try {
        const result = await api('/api/chat/new', { method: 'POST', body: { phone, body } });
        hideNewChatDialog();
        // Reload conversations and select the new one
        await loadConversations();
        if (result.conversationId) {
            selectConversation(result.conversationId);
        }
    } catch (e) {
        document.getElementById('new-chat-error').textContent = e.message || 'Could not start conversation';
        document.getElementById('new-chat-error').classList.remove('hidden');
    }
}

// ─── Media ───────────────────────────────────────────────────────────────────
function getMediaProxyUrl(msg, convId) {
    if (!msg.mediaId || msg.mediaId === '00000000-0000-0000-0000-000000000000') return null;
    // uuid may be missing from conversation detail responses;
    // fall back to messageId (same strategy as reference implementation)
    const uuid = msg.uuid || msg.messageId;
    const params = new URLSearchParams({
        uuid: uuid,
        mediaId: msg.mediaId,
        messageId: msg.messageId,
        conversationId: convId,
        mediaType: msg.mediaType || 'ImageAvif',
    });
    return `/api/media/proxy?${params}`;
}

// Stage a file for preview before sending (allows adding caption)
function handleFileSelect() {
    const input = document.getElementById('file-input');
    if (input.files.length > 0) {
        stageMediaPreview(input.files[0]);
        input.value = '';
    }
}

let stagedMediaFile = null;

function stageMediaPreview(file) {
    stagedMediaFile = file;
    const preview = document.getElementById('media-preview');
    const previewImg = document.getElementById('media-preview-img');
    const previewAudio = document.getElementById('media-preview-audio');
    const msgInput = document.getElementById('message-input');

    previewImg.classList.add('hidden');
    previewAudio.classList.add('hidden');

    if (file.type.startsWith('image/')) {
        const url = URL.createObjectURL(file);
        previewImg.src = url;
        previewImg.classList.remove('hidden');
        previewImg.onload = () => URL.revokeObjectURL(url);
    } else if (file.type.startsWith('audio/')) {
        const audioUrl = URL.createObjectURL(file);
        previewAudio.innerHTML = `<audio controls preload="metadata" style="max-width:200px;height:32px"><source src="${audioUrl}"></audio>`;
        previewAudio.classList.remove('hidden');
    }

    preview.classList.remove('hidden');
    msgInput.placeholder = 'Add a caption... (optional)';
    msgInput.focus();
}

function cancelMediaPreview() {
    stagedMediaFile = null;
    const preview = document.getElementById('media-preview');
    const msgInput = document.getElementById('message-input');
    preview.classList.add('hidden');
    msgInput.placeholder = 'Type a message...';
}

async function sendMediaFile(file, caption) {
    if (!state.currentConversationId) return;
    const convId = state.currentConversationId;
    const to = getRecipientPhones(convId);
    if (to.length === 0) {
        console.error('No recipient phone numbers found');
        return;
    }

    const tempId = 'sending-media-' + Date.now();
    const label = caption || (file.type.startsWith('image/') ? 'Sending image...' : 'Sending audio...');
    addOptimisticMessage(convId, tempId, label, 'sending');

    const form = new FormData();
    form.append('file', file);
    form.append('to', JSON.stringify(to));
    form.append('conversationId', convId);
    if (caption) form.append('body', caption);

    try {
        const resp = await fetch('/api/media/send', { method: 'POST', body: form });
        const data = await resp.json();
        if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
        // Swap temp ID for real ID and flag that this message needs media.
        // When the SSE status update confirms "Sent", handleStatusUpdate()
        // will fetch the full message (with mediaId) and rebuild the DOM.
        replaceOptimisticMessage(convId, tempId, data.messageId, 'sent');
        const msg = state.messages.find(m => m.messageId === data.messageId);
        if (msg) msg._needsMedia = true;
    } catch (e) {
        console.error('Failed to send media:', e);
        markOptimisticFailed(tempId, e.message || 'Failed to send media');
    }
}

// ─── Audio Recording ────────────────────────────────────────────────────────
let mediaRecorder = null;
let audioChunks = [];
let recordingStartTime = 0;
let recordingTimer = null;

async function toggleRecording() {
    if (mediaRecorder && mediaRecorder.state === 'recording') {
        stopRecording();
        return;
    }
    startRecording();
}

async function startRecording() {
    // Check secure context (getUserMedia requires HTTPS or localhost)
    if (!window.isSecureContext) {
        alert('Voice recording requires HTTPS. Please access this site via HTTPS or localhost.');
        return;
    }
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        alert('Your browser does not support audio recording.');
        return;
    }

    // Check existing permission state before prompting
    try {
        const permStatus = await navigator.permissions.query({ name: 'microphone' });
        if (permStatus.state === 'denied') {
            alert('Microphone access is blocked. Please allow microphone access in your browser settings and reload.');
            return;
        }
    } catch (e) {
        // permissions API not supported, proceed anyway
    }

    try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        // Pick the best supported audio format for recording.
        // Matches gomuks priority: ogg/opus first (Firefox native),
        // then webm/opus (Chrome), then mp4 (Safari).
        let mimeType = '';
        let fileExt = '';
        for (const [mime, ext] of [
            ['audio/ogg; codecs=opus', 'ogg'],
            ['audio/ogg;codecs=opus', 'ogg'],
            ['audio/webm;codecs=opus', 'webm'],
            ['audio/webm', 'webm'],
            ['audio/mp4', 'mp4'],
            ['audio/aac', 'aac'],
        ]) {
            if (MediaRecorder.isTypeSupported(mime)) {
                mimeType = mime;
                fileExt = ext;
                break;
            }
        }
        if (!mimeType) {
            alert('Your browser does not support audio recording in any known format.');
            stream.getTracks().forEach(t => t.stop());
            return;
        }
        console.log('Recording with MIME type:', mimeType);

        mediaRecorder = new MediaRecorder(stream, { mimeType });
        audioChunks = [];

        mediaRecorder.ondataavailable = (e) => {
            if (e.data.size > 0) audioChunks.push(e.data);
        };

        mediaRecorder.onstop = () => {
            stream.getTracks().forEach(t => t.stop());
            clearInterval(recordingTimer);
            const blob = new Blob(audioChunks, { type: mimeType });
            const file = new File([blob], `voice.${fileExt}`, { type: mimeType });
            stageMediaPreview(file);
            updateRecordingUI(false);
        };

        mediaRecorder.start();
        recordingStartTime = Date.now();
        updateRecordingUI(true);
        recordingTimer = setInterval(updateRecordingDuration, 100);

        // Auto-stop after 30 seconds (Garmin limit)
        setTimeout(() => {
            if (mediaRecorder && mediaRecorder.state === 'recording') {
                stopRecording();
            }
        }, 30000);
    } catch (e) {
        console.error('Microphone access error:', e);
        if (e.name === 'NotAllowedError') {
            alert('Microphone access was denied. Please allow microphone access in your browser and try again.');
        } else if (e.name === 'NotFoundError') {
            alert('No microphone found. Please connect a microphone and try again.');
        } else {
            alert('Could not access microphone: ' + e.message);
        }
    }
}

function stopRecording() {
    if (mediaRecorder && mediaRecorder.state === 'recording') {
        mediaRecorder.stop();
    }
}

function updateRecordingUI(recording) {
    const btn = document.getElementById('record-btn');
    const duration = document.getElementById('recording-duration');
    if (recording) {
        btn.classList.add('recording');
        btn.innerHTML = '&#9632;'; // stop square
        duration.classList.remove('hidden');
    } else {
        btn.classList.remove('recording');
        btn.innerHTML = '&#127908;'; // microphone
        duration.classList.add('hidden');
        duration.textContent = '';
    }
}

function updateRecordingDuration() {
    const elapsed = Math.floor((Date.now() - recordingStartTime) / 1000);
    const mins = Math.floor(elapsed / 60);
    const secs = elapsed % 60;
    const el = document.getElementById('recording-duration');
    if (el) el.textContent = `${mins}:${secs.toString().padStart(2, '0')}`;
}

// ─── SSE ─────────────────────────────────────────────────────────────────────
function connectSSE() {
    if (state.eventSource) state.eventSource.close();

    const es = new EventSource('/api/events');
    state.eventSource = es;

    es.addEventListener('message', (e) => {
        const msg = JSON.parse(e.data);
        handleIncomingMessage(msg);
    });

    es.addEventListener('status', (e) => {
        const update = JSON.parse(e.data);
        handleStatusUpdate(update);
    });

    es.addEventListener('connected', () => {
        console.log('SignalR connected');
    });

    es.addEventListener('disconnected', () => {
        console.log('SignalR disconnected');
    });

    es.addEventListener('error', (e) => {
        console.error('SSE error, will auto-reconnect');
    });

    // Catch up on tab focus — lightweight diff, no full re-render
    document.addEventListener('visibilitychange', () => {
        if (!document.hidden && state.loggedIn) {
            loadConversations();
            if (state.currentConversationId) {
                catchUpConversation(state.currentConversationId);
            }
        }
    });
}

// Track unread messages per conversation
const unreadCounts = {};
let lastHandledMessageId = '';

// Persistent "last seen" per conversation — survives page close
function getLastSeen(convId) {
    try { return localStorage.getItem('gm_seen_' + convId) || ''; } catch { return ''; }
}
function setLastSeen(convId) {
    try { localStorage.setItem('gm_seen_' + convId, new Date().toISOString()); } catch {}
}

// On load: mark conversations as unread if updatedDate > lastSeen
function computeOfflineUnread() {
    for (const conv of state.conversations) {
        const lastSeen = getLastSeen(conv.conversationId);
        if (!lastSeen) continue; // never opened — don't badge
        if (conv.updatedDate && conv.updatedDate > lastSeen) {
            // Don't override live SSE counts
            if (!unreadCounts[conv.conversationId]) {
                unreadCounts[conv.conversationId] = -1; // -1 = "dot" (unknown count)
            }
        }
    }
}

function handleIncomingMessage(msg) {
    const convId = msg.conversationId;
    const isReaction = isReactionMessage(msg);
    console.log('handleIncomingMessage:', msg.messageId, 'isReaction:', isReaction,
        'body:', msg.messageBody ? JSON.stringify(msg.messageBody.slice(0, 80)) : null,
        'from:', msg.from, 'isMine:', isMine(msg));

    // Deduplicate: same message can arrive via both SignalR and FCM
    if (msg.messageId && msg.messageId === lastHandledMessageId) return;
    if (msg.messageId) lastHandledMessageId = msg.messageId;

    // Track unread for conversations we're not currently viewing (skip own messages)
    if (state.currentConversationId === convId) {
        setLastSeen(convId); // keep "last seen" fresh for the active conversation
    } else if (!isMine(msg)) {
        unreadCounts[convId] = (unreadCounts[convId] || 0) + 1;
    }

    // Update conversation list (move to top)
    const idx = state.conversations.findIndex(c => c.conversationId === convId);
    if (idx >= 0) {
        state.conversations[idx].updatedDate = new Date().toISOString();
        state.conversations.sort((a, b) => new Date(b.updatedDate) - new Date(a.updatedDate));
    } else {
        // New conversation - reload
        loadConversations();
    }
    // Update conversation cache with live data
    cache.set('conversations', state.conversations);
    renderConversations();

    // If viewing this conversation, handle the message
    if (state.currentConversationId === convId) {
        const existing = state.messages.find(m => m.messageId === msg.messageId);
        if (existing) {
            // Message ID already in state (e.g. from replaceOptimisticMessage after
            // media send). Merge server data (mediaId, mediaType, etc.) and rebuild
            // the DOM element so media/audio actually renders.
            const hadMedia = existing.mediaId;
            Object.assign(existing, msg);
            delete existing._sendState;
            delete existing._errorMsg;
            delete existing._needsMedia;
            if (!hadMedia && existing.mediaId) {
                rebuildMessageDOM(existing.messageId);
            }
        } else {
            state.messages.push(msg);
            cache.set('msgs_' + convId, state.messages);

            if (isReactionMessage(msg)) {
                // Reaction messages aren't shown in timeline — update target's badges
                const r = extractReaction(msg);
                console.log('  Reaction extracted:', r);
                if (r) {
                    const target = findReactionTarget(r, msg);
                    console.log('  Reaction target:', target ? target.messageId : 'NOT FOUND');
                    if (target) {
                        // Clear matching optimistic reaction (server confirmed it)
                        if (target._reactions) {
                            const idx = target._reactions.findIndex(rx => rx.emoji === r.emoji && rx.isMine);
                            if (idx >= 0) target._reactions.splice(idx, 1);
                        }
                        updateMessageReactions(target.messageId);
                    }
                }
            } else {
                appendMessageToDOM(msg);
                scrollToBottom();
            }
            markAsRead(convId, msg.messageId);
        }
    }
}

function handleStatusUpdate(update) {
    if (!update.messageId) return;
    const msgId = update.messageId.messageId;
    const msg = state.messages.find(m => m.messageId === msgId);
    if (msg && update.messageStatus) {
        if (!msg.status) msg.status = [];
        const existing = msg.status.find(s => s.userId === (update.userId || ''));
        if (existing) {
            existing.messageStatus = update.messageStatus;
        } else {
            msg.status.push({ userId: update.userId || '', messageStatus: update.messageStatus });
        }
        updateMessageStatus(msgId); // surgical DOM update — no re-render

        // When Garmin confirms a media message as Sent, the media attachment
        // is now available on the server. Fetch the full message to get
        // mediaId/mediaType and rebuild the DOM to show the actual image
        // or waveform player.
        if (msg._needsMedia && (update.messageStatus === 'Sent' || update.messageStatus === 'Delivered')) {
            delete msg._needsMedia;
            if (state.currentConversationId) {
                fetchMediaForMessage(state.currentConversationId, msgId);
            }
        }
    }
}

// ─── Rendering ───────────────────────────────────────────────────────────────
function renderConversations() {
    const list = document.getElementById('conversation-list');
    if (state.conversations.length === 0) {
        list.innerHTML = '<div class="empty-state" style="height:200px">No conversations</div>';
        return;
    }

    let html = state.conversations.map(conv => {
        const active = conv.conversationId === state.currentConversationId ? 'active' : '';
        const name = getConversationName(conv);
        const initial = name.charAt(0).toUpperCase();
        const time = formatTime(conv.updatedDate);
        const unread = unreadCounts[conv.conversationId] || 0;
        const unreadBadge = unread > 0
            ? `<span class="unread-badge">${unread}</span>`
            : unread === -1
                ? `<span class="unread-badge">&bull;</span>`
                : '';

        return `
            <div class="conversation-item ${active}" onclick="selectConversation('${conv.conversationId}')">
                <div class="conversation-avatar">${initial}</div>
                <div class="conversation-info">
                    <div class="conversation-name">${escapeHtml(name)}</div>
                </div>
                <div class="conversation-meta">
                    <div class="conversation-time">${time}</div>
                    ${unreadBadge}
                </div>
            </div>
        `;
    }).join('');

    if (hasMoreConversations) {
        html += `<div class="load-more" onclick="loadMoreConversations()">Load more conversations...</div>`;
    }

    list.innerHTML = html;
}

// Detect Garmin ZWS-encoded reaction: \u200b{emoji}\u200b to \u200a{text}\u200a
function isReactionMessage(msg) {
    const body = msg.messageBody || '';
    return body.startsWith('\u200b');
}

function extractReaction(msg) {
    const body = msg.messageBody || '';
    // Match ZWS-delimited emoji and hair-space-delimited target text,
    // ignoring the localized connector word(s) in between.
    // Strips optional guillemets «» that iOS may add.
    var match = body.match(/^\u200b(.+?)\u200b .+[«]?\u200a(.*?)\u200a[»]?$/);
    if (!match) return null;
    var targetText = match[2].replace(/\u2026$/, '').replace(/\.\.\.$/,'');
    return { emoji: match[1], targetText: targetText };
}

// Find the target message for a reaction by matching body text,
// preferring the message closest in time (before the reaction).
function findReactionTarget(r, reactionMsg) {
    const reactionTime = new Date(reactionMsg.sentAt || reactionMsg.receivedAt || 0).getTime();
    let target = null;
    let bestTimeDiff = Infinity;
    for (const candidate of state.messages) {
        if (candidate.messageId === reactionMsg.messageId) continue;
        if (isReactionMessage(candidate)) continue;
        const candidateBody = (candidate.messageBody || '').replace(/[\u200a\u200b\u2009]/g, '').trim();
        // Exact match, or startsWith for truncated reactions from iOS
        if (candidateBody !== r.targetText && !candidateBody.startsWith(r.targetText)) continue;
        if (!r.targetText) continue; // don't match empty
        const candidateTime = new Date(candidate.sentAt || candidate.receivedAt || 0).getTime();
        const diff = Math.abs(reactionTime - candidateTime);
        if (diff < bestTimeDiff) {
            bestTimeDiff = diff;
            target = candidate;
        }
    }
    return target;
}

function renderMessages() {
    const container = document.getElementById('messages');
    if (state.messages.length === 0) {
        container.innerHTML = '<div class="empty-state">No messages</div>';
        return;
    }

    // First pass: collect reactions and map them to target messages.
    // Match by body text, preferring the message closest in time to the reaction.
    const reactions = {}; // messageId -> [{emoji, from}]
    const reactionMsgIds = new Set();

    for (const msg of state.messages) {
        if (!isReactionMessage(msg)) continue;
        const r = extractReaction(msg);
        if (!r) continue;
        reactionMsgIds.add(msg.messageId);

        const target = findReactionTarget(r, msg);
        if (target) {
            if (!reactions[target.messageId]) reactions[target.messageId] = [];
            reactions[target.messageId].push({
                emoji: r.emoji,
                from: msg.from,
                isMine: isMine(msg),
            });
        }
    }

    // Second pass: render messages, skipping reaction messages
    let html = '';
    html += '<div style="flex:1 0 0;"></div>';

    // "Load older messages" button at top
    if (state.messages.length >= 20) {
        html += `<div class="load-more" onclick="loadOlderMessages()">Load older messages...</div>`;
    }

    let lastDate = '';

    for (const msg of state.messages) {
        if (reactionMsgIds.has(msg.messageId)) continue; // skip reaction messages

        const date = formatDate(msg.sentAt || msg.receivedAt);
        if (date !== lastDate) {
            html += `<div class="message-time-separator">${date}</div>`;
            lastDate = date;
        }

        html += renderSingleMessage(msg, reactions);
    }

    container.innerHTML = html;

    // Load media asynchronously after rendering
    loadMediaForMessages();
}

// Render a single message to HTML string.
function renderSingleMessage(msg, reactions) {
    const isSent = isMine(msg);
    const cls = isSent ? 'sent' : 'received';
    const sendState = msg._sendState || '';
    const failedCls = sendState === 'failed' ? ' message-failed' : '';
    const sendingCls = sendState === 'sending' ? ' message-sending' : '';
    const senderName = !isSent ? getSenderName(msg) : null;
    const body = getMessageBody(msg);
    const time = formatMessageTime(msg.sentAt || msg.receivedAt);
    const location = getLocationHtml(msg);
    const device = getDeviceLabel(msg);
    const mediaHtml = getMediaHtml(msg, state.currentConversationId);
    const transcription = msg.transcription
        ? `<div class="message-transcription">${escapeHtml(msg.transcription)}</div>` : '';

    let statusIcon = '';
    if (sendState === 'sending') {
        statusIcon = '...';
    } else if (sendState === 'failed') {
        statusIcon = '!';
    } else if (isSent) {
        statusIcon = getStatusIcon(msg);
    }

    const errorHtml = msg._errorMsg
        ? `<div class="message-error">${escapeHtml(msg._errorMsg)}</div>` : '';

    const msgReactions = [...((reactions || {})[msg.messageId] || []), ...(msg._reactions || [])];
    let reactionsHtml = '';
    if (msgReactions.length > 0) {
        const counts = {};
        for (const r of msgReactions) {
            counts[r.emoji] = (counts[r.emoji] || 0) + 1;
        }
        const badges = Object.entries(counts).map(([emoji, count]) =>
            `<span class="reaction-badge">${emoji}${count > 1 ? ' ' + count : ''}</span>`
        ).join('');
        reactionsHtml = `<div class="reaction-badges">${badges}</div>`;
    }

    return `
        <div class="message ${cls}${failedCls}${sendingCls}" data-msgid="${msg.messageId}">
            ${senderName ? `<div class="message-sender">${escapeHtml(senderName)}</div>` : ''}
            <div class="message-bubble">${mediaHtml}${body ? `<div class="message-text">${escapeHtml(body)}</div>` : ''}${location}${transcription}</div>
            ${reactionsHtml}
            <div class="message-meta">
                <span>${time}</span>
                ${device ? `<span class="message-device">${device}</span>` : ''}
                ${statusIcon ? `<span class="message-status ${sendState === 'failed' ? 'failed' : getStatusClass(msg)}">${statusIcon}</span>` : ''}
                ${!sendState ? `<button class="react-btn" onclick="showReactionPicker('${msg.messageId}')" title="React">+</button>` : ''}
            </div>
            ${errorHtml}
        </div>`;
}

// Append a single message to the DOM without re-rendering everything.
function appendMessageToDOM(msg) {
    const container = document.getElementById('messages');
    const div = document.createElement('div');
    div.innerHTML = renderSingleMessage(msg, {});
    container.appendChild(div.firstElementChild);
    // Load media for just this message
    if (msg.mediaId && msg.mediaId !== '00000000-0000-0000-0000-000000000000' && msg.mediaType) {
        loadMediaForMessages();
    }
    scrollToBottom();
}


// ─── Helpers ─────────────────────────────────────────────────────────────────
function getConversationName(conv) {
    const members = state.members[conv.conversationId] || [];
    const otherMembers = members.filter(m => {
        const id = (m.userIdentifier || '').toLowerCase();
        const addr = m.address || '';
        return id !== state.userId && addr !== state.phone;
    });

    if (otherMembers.length > 0) {
        return otherMembers.map(m => m.friendlyName || m.address || 'Unknown').join(', ');
    }

    // Self-chat: all members are us — show our own name/number
    if (members.length > 0) {
        const me = members.find(m => {
            const id = (m.userIdentifier || '').toLowerCase();
            return id === state.userId || m.address === state.phone;
        });
        if (me) {
            return (me.friendlyName || me.address || state.phone) + ' (you)';
        }
    }

    // Fallback: use member IDs, lookup name by lowercase UUID
    const otherIds = conv.memberIds.filter(id => id.toLowerCase() !== state.userId);
    if (otherIds.length === 0 && conv.memberIds.length > 0) {
        return state.phone + ' (you)';
    }
    return otherIds.map(id => {
        return state.memberNames[id] || state.memberNames[id.toLowerCase()] || id.substring(0, 8) + '...';
    }).join(', ');
}

function updateConversationTitle(convId) {
    const conv = state.conversations.find(c => c.conversationId === convId);
    if (conv) {
        document.getElementById('conversation-title').textContent = getConversationName(conv);
    }
}

function isMine(msg) {
    if (!msg.from) return false;
    const from = msg.from.toLowerCase();
    if (from === state.userId) return true;
    if (from === state.phone) return true;
    return false;
}

function getSenderName(msg) {
    const from = msg.from;
    if (!from) return null;
    const fromLower = from.toLowerCase();
    // Try cached name lookup first
    if (state.memberNames[from]) return state.memberNames[from];
    if (state.memberNames[fromLower]) return state.memberNames[fromLower];
    // Fallback: search loaded members for this conversation
    const convMembers = state.members[msg.conversationId];
    if (convMembers) {
        for (const m of convMembers) {
            if (m.userIdentifier && m.userIdentifier.toLowerCase() === fromLower) {
                const name = m.friendlyName || m.address || from;
                state.memberNames[fromLower] = name;
                return name;
            }
        }
    }
    // Last resort: show phone-like address if available from members, not raw UUID
    return from;
}

function getMessageBody(msg) {
    let body = msg.messageBody || '';
    // Strip ZWS reaction encoding
    body = body.replace(/[\u200a\u200b\u2009]/g, '').trim();
    if (msg.transcription) {
        body = (body ? body + ' ' : '') + msg.transcription;
    }
    // Don't show "(no text)" for media-only messages
    if (!body && msg.mediaId) return '';
    return body || '(no text)';
}

function getLocationHtml(msg) {
    const loc = msg.userLocation;
    if (!loc || loc.latitudeDegrees == null || loc.longitudeDegrees == null) return '';
    const lat = loc.latitudeDegrees;
    const lon = loc.longitudeDegrees;
    const osmUrl = `https://www.openstreetmap.org/?mlat=${lat}&mlon=${lon}#map=15/${lat}/${lon}`;
    let extra = '';
    if (loc.elevationMeters != null) extra += ` ${Math.round(loc.elevationMeters)}m`;
    return `<div class="message-location"><a href="${osmUrl}" target="_blank" rel="noopener">📍 ${lat.toFixed(5)}, ${lon.toFixed(5)}${extra}</a></div>`;
}

// Cache of media proxy URLs keyed by messageId (survives re-renders)
const mediaUrlCache = {};
// Cache of waveform amplitude data keyed by URL (avoids re-decoding audio)
const waveformCache = {};

function mediaSizeStyle(msg) {
    // Pre-calculate container size from metadata to prevent layout shift
    var meta = msg.mediaMetadata;
    if (!meta || !meta.width || !meta.height) {
        // No metadata — let the image size naturally, stabilizeScroll handles it
        return '';
    }
    var w = meta.width, h = meta.height;
    var maxW = 300, maxH = 300;
    if (w > maxW) { h = Math.round(h * maxW / w); w = maxW; }
    if (h > maxH) { w = Math.round(w * maxH / h); h = maxH; }
    return 'min-width:' + w + 'px;min-height:' + h + 'px;';
}

function getMediaHtml(msg, convId) {
    // Skip messages without media or with nil UUID mediaId
    if (!msg.mediaId || msg.mediaId === '00000000-0000-0000-0000-000000000000') return '';
    if (!msg.mediaType) return '';
    const msgId = msg.messageId;
    const sizeStyle = mediaSizeStyle(msg);

    // Check if we already have the URL cached
    const cachedUrl = mediaUrlCache[msgId];
    if (cachedUrl) {
        if (msg.mediaType === 'ImageAvif') {
            return `<div class="message-image-container" style="${sizeStyle}"><img class="message-image" src="${escapeHtml(cachedUrl)}" alt="Image" onclick="openLightbox('${escapeHtml(cachedUrl)}')" onload="stabilizeScroll()"></div>`;
        }
        if (msg.mediaType === 'AudioOgg') {
            return `<div class="message-audio" id="media-${msgId}"></div>`;
        }
    }

    // Not cached yet — show placeholder with reserved size
    if (msg.mediaType === 'ImageAvif') {
        return `<div class="message-image-container" id="media-${msgId}" style="${sizeStyle}"></div>`;
    }
    if (msg.mediaType === 'AudioOgg') {
        return `<div class="message-audio" id="media-${msgId}"></div>`;
    }
    return '';
}

// Only processes messages that need async media loading or waveform init.
// Cached images are already rendered inline by getMediaHtml() — skipped here.
function loadMediaForMessages() {
    const convId = state.currentConversationId;
    if (!convId) return;

    for (const msg of state.messages) {
        if (!msg.mediaId || msg.mediaId === '00000000-0000-0000-0000-000000000000' || !msg.mediaType) continue;

        const msgId = msg.messageId;
        const el = document.getElementById(`media-${msgId}`);
        if (!el) continue;
        if (el.dataset.loaded) continue;

        const url = mediaUrlCache[msgId] || getMediaProxyUrl(msg, convId);
        if (!url) continue;
        mediaUrlCache[msgId] = url;
        el.dataset.loaded = 'true';

        if (msg.mediaType === 'ImageAvif') {
            el.innerHTML = `<img class="message-image" src="${escapeHtml(url)}" alt="Image" onclick="openLightbox('${escapeHtml(url)}')" onload="stabilizeScroll()">`;
        } else if (msg.mediaType === 'AudioOgg') {
            try {
                createWaveformPlayer(el, url);
            } catch (e) {
                // Fallback to basic audio player if waveform fails
                el.innerHTML = `<audio controls preload="metadata" style="max-width:250px;height:36px"><source src="${escapeHtml(url)}" type="audio/ogg"></audio>`;
            }
        }
    }
}

// ─── Waveform Audio Player ──────────────────────────────────────────────────
function createWaveformPlayer(container, audioUrl) {
    const playerId = 'player-' + Math.random().toString(36).slice(2);
    container.innerHTML = `
        <div class="waveform-player" id="${playerId}">
            <button class="waveform-play-btn" title="Play">&#9654;</button>
            <div class="waveform-bars"></div>
            <span class="waveform-time">0:00</span>
        </div>
    `;

    const player = document.getElementById(playerId);
    const playBtn = player.querySelector('.waveform-play-btn');
    const barsContainer = player.querySelector('.waveform-bars');
    const timeDisplay = player.querySelector('.waveform-time');

    const audio = new Audio();
    audio.preload = 'none';
    audio.src = audioUrl;

    const barCount = 40;
    let bars = [];
    let isPlaying = false;

    // Create placeholder bars (flat until real waveform loads)
    for (let i = 0; i < barCount; i++) {
        const bar = document.createElement('div');
        bar.className = 'waveform-bar';
        bar.style.height = '15%';
        barsContainer.appendChild(bar);
        bars.push(bar);
    }

    // Apply waveform data (from cache or by decoding audio)
    function applyWaveform(heights, durationSecs) {
        for (let i = 0; i < barCount && i < heights.length; i++) {
            bars[i].style.height = heights[i] + '%';
        }
        if (durationSecs) {
            const mins = Math.floor(durationSecs / 60);
            const secs = Math.floor(durationSecs % 60);
            timeDisplay.textContent = mins + ':' + secs.toString().padStart(2, '0');
        }
    }

    if (waveformCache[audioUrl]) {
        // Use cached waveform data (no network/decode needed)
        const c = waveformCache[audioUrl];
        applyWaveform(c.heights, c.duration);
    } else {
        // Decode audio to generate waveform
        (async function() {
            try {
                const resp = await fetch(audioUrl);
                const arrayBuf = await resp.arrayBuffer();
                const audioCtx = new (window.AudioContext || window.webkitAudioContext)();
                const decoded = await audioCtx.decodeAudioData(arrayBuf);
                const channelData = decoded.getChannelData(0);
                audioCtx.close();

                const samplesPerBar = Math.floor(channelData.length / barCount);
                const heights = [];
                let maxAmp = 0;
                const amps = [];
                for (let i = 0; i < barCount; i++) {
                    let sumSq = 0;
                    const start = i * samplesPerBar;
                    for (let j = start; j < start + samplesPerBar && j < channelData.length; j++) {
                        sumSq += channelData[j] * channelData[j];
                    }
                    const rms = Math.sqrt(sumSq / samplesPerBar);
                    amps.push(rms);
                    if (rms > maxAmp) maxAmp = rms;
                }
                for (let i = 0; i < barCount; i++) {
                    heights.push(Math.max(8, (maxAmp > 0 ? amps[i] / maxAmp : 0) * 100));
                }

                waveformCache[audioUrl] = { heights, duration: decoded.duration };
                applyWaveform(heights, decoded.duration);
            } catch (e) {
                // Keep placeholder bars on error
            }
        })();
    }

    playBtn.onclick = () => {
        if (isPlaying) {
            audio.pause();
        } else {
            document.querySelectorAll('.waveform-player.playing').forEach(p => {
                if (p !== player) {
                    const otherBtn = p.querySelector('.waveform-play-btn');
                    if (otherBtn) otherBtn.click();
                }
            });
            audio.play();
        }
    };

    audio.onplay = () => {
        isPlaying = true;
        player.classList.add('playing');
        playBtn.innerHTML = '&#9646;&#9646;'; // pause
    };

    audio.onpause = () => {
        isPlaying = false;
        player.classList.remove('playing');
        playBtn.innerHTML = '&#9654;'; // play
    };

    audio.onended = () => {
        isPlaying = false;
        player.classList.remove('playing');
        playBtn.innerHTML = '&#9654;';
        updateProgress(0);
    };

    audio.ontimeupdate = () => {
        if (!audio.duration) return;
        const progress = audio.currentTime / audio.duration;
        updateProgress(progress);
        const remaining = audio.duration - audio.currentTime;
        const mins = Math.floor(remaining / 60);
        const secs = Math.floor(remaining % 60);
        timeDisplay.textContent = mins + ':' + secs.toString().padStart(2, '0');
    };

    audio.onloadedmetadata = () => {
        const dur = audio.duration;
        const mins = Math.floor(dur / 60);
        const secs = Math.floor(dur % 60);
        timeDisplay.textContent = mins + ':' + secs.toString().padStart(2, '0');
    };

    function updateProgress(progress) {
        const activeCount = Math.round(progress * barCount);
        for (let i = 0; i < barCount; i++) {
            bars[i].classList.toggle('active', i <= activeCount);
        }
    }

    // Click on waveform to seek
    barsContainer.onclick = (e) => {
        if (!audio.duration) return;
        const rect = barsContainer.getBoundingClientRect();
        const x = e.clientX - rect.left;
        const progress = x / rect.width;
        audio.currentTime = progress * audio.duration;
        if (!isPlaying) audio.play();
    };
}

function getDeviceLabel(msg) {
    if (!msg.fromDeviceType) return '';
    switch (msg.fromDeviceType) {
        case 'inReach': return '📡';
        case 'GarminOSApp': return '⌚';
        default: return '';
    }
}

function getStatusIcon(msg) {
    if (!msg.status || msg.status.length === 0) return '✓';
    const statuses = msg.status.map(s => s.messageStatus);
    if (statuses.includes('Read')) return '✓✓';
    if (statuses.includes('Delivered')) return '✓✓';
    if (statuses.includes('Sent')) return '✓';
    if (statuses.includes('Undeliverable')) return '✗';
    return '✓';
}

function getStatusClass(msg) {
    if (!msg.status) return '';
    const statuses = msg.status.map(s => s.messageStatus);
    if (statuses.includes('Read')) return 'read';
    return '';
}

// ─── Surgical DOM Updates (no full re-render) ───────────────────────────────

function updateMessageStatus(msgId) {
    const el = document.querySelector(`[data-msgid="${msgId}"]`);
    if (!el) return;
    const msg = state.messages.find(m => m.messageId === msgId);
    if (!msg || !isMine(msg)) return;
    const statusEl = el.querySelector('.message-status');
    if (!statusEl) return;
    statusEl.textContent = getStatusIcon(msg);
    statusEl.className = 'message-status ' + getStatusClass(msg);
}

function collectReactionsForMessage(targetMsg) {
    const results = [];
    const targetBody = (targetMsg.messageBody || '').replace(/[\u200a\u200b\u2009]/g, '').trim();
    if (!targetBody) return results;
    const targetTime = new Date(targetMsg.sentAt || targetMsg.receivedAt || 0).getTime();
    for (const msg of state.messages) {
        if (!isReactionMessage(msg)) continue;
        const r = extractReaction(msg);
        if (!r || !r.targetText) continue;
        if (r.targetText !== targetBody && !targetBody.startsWith(r.targetText)) continue;
        results.push({ emoji: r.emoji, from: msg.from, isMine: isMine(msg) });
    }
    return results;
}

function updateMessageReactions(msgId) {
    const el = document.querySelector(`[data-msgid="${msgId}"]`);
    if (!el) return;
    const msg = state.messages.find(m => m.messageId === msgId);
    if (!msg) return;

    const combined = [...collectReactionsForMessage(msg), ...(msg._reactions || [])];
    let badgesHtml = '';
    if (combined.length > 0) {
        const counts = {};
        for (const r of combined) counts[r.emoji] = (counts[r.emoji] || 0) + 1;
        const badges = Object.entries(counts).map(([emoji, count]) =>
            `<span class="reaction-badge">${emoji}${count > 1 ? ' ' + count : ''}</span>`
        ).join('');
        badgesHtml = `<div class="reaction-badges">${badges}</div>`;
    }

    const existing = el.querySelector('.reaction-badges');
    if (existing) {
        if (badgesHtml) { existing.outerHTML = badgesHtml; } else { existing.remove(); }
    } else if (badgesHtml) {
        const meta = el.querySelector('.message-meta');
        if (meta) meta.insertAdjacentHTML('beforebegin', badgesHtml);
    }
}

function updateMessageFailed(msgId) {
    const el = document.querySelector(`[data-msgid="${msgId}"]`);
    if (!el) return;
    const msg = state.messages.find(m => m.messageId === msgId);
    if (!msg) return;
    el.classList.add('message-failed');
    el.classList.remove('message-sending');
    const statusEl = el.querySelector('.message-status');
    if (statusEl) { statusEl.textContent = '!'; statusEl.className = 'message-status failed'; }
    if (msg._errorMsg && !el.querySelector('.message-error')) {
        el.insertAdjacentHTML('beforeend', `<div class="message-error">${escapeHtml(msg._errorMsg)}</div>`);
    }
}

// Rebuild a single message's DOM element in-place (e.g. when server data
// arrives with media that the optimistic placeholder didn't have).
function rebuildMessageDOM(msgId) {
    const el = document.querySelector(`[data-msgid="${msgId}"]`);
    if (!el) return;
    const msg = state.messages.find(m => m.messageId === msgId);
    if (!msg) return;
    const tmp = document.createElement('div');
    tmp.innerHTML = renderSingleMessage(msg, {});
    const newEl = tmp.firstElementChild;
    if (newEl) {
        el.replaceWith(newEl);
        if (msg.mediaId && msg.mediaId !== '00000000-0000-0000-0000-000000000000' && msg.mediaType) {
            loadMediaForMessages();
        }
    }
}

function formatTime(dateStr) {
    if (!dateStr) return '';
    const d = new Date(dateStr);
    const now = new Date();
    const diff = now - d;

    if (diff < 86400000 && d.getDate() === now.getDate()) {
        return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    }
    if (diff < 604800000) {
        return d.toLocaleDateString([], { weekday: 'short' });
    }
    return d.toLocaleDateString([], { day: 'numeric', month: 'short' });
}

function formatDate(dateStr) {
    if (!dateStr) return '';
    const d = new Date(dateStr);
    const now = new Date();
    if (d.toDateString() === now.toDateString()) return 'Today';
    const yesterday = new Date(now);
    yesterday.setDate(yesterday.getDate() - 1);
    if (d.toDateString() === yesterday.toDateString()) return 'Yesterday';
    return d.toLocaleDateString([], { weekday: 'long', day: 'numeric', month: 'long' });
}

function formatMessageTime(dateStr) {
    if (!dateStr) return '';
    return new Date(dateStr).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// Called when an image finishes loading — prevents scroll jump.
// If user was near bottom, stay at bottom. Otherwise hold position.
function stabilizeScroll() {
    const container = document.getElementById('messages');
    const nearBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 200;
    if (nearBottom) {
        container.scrollTop = container.scrollHeight;
    }
}

function openLightbox(url) {
    const overlay = document.createElement('div');
    overlay.className = 'lightbox';
    overlay.innerHTML = `<img src="${url}" alt="Full size">`;
    overlay.onclick = () => overlay.remove();
    document.body.appendChild(overlay);
}

function escapeHtml(str) {
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

// Only auto-scroll if user is already near the bottom (within 150px).
// This prevents jumping around when reading history.
function scrollToBottom(force) {
    const container = document.getElementById('messages');
    // Double rAF ensures layout is complete before scrolling (important on mobile)
    requestAnimationFrame(() => {
        requestAnimationFrame(() => {
            const nearBottom = container.scrollHeight - container.scrollTop - container.clientHeight < 150;
            if (force || nearBottom) {
                container.scrollTop = container.scrollHeight;
            }
        });
    });
}

// Force-scroll (used when selecting a conversation or sending)
function scrollToBottomForce() {
    scrollToBottom(true);
}


function showChatView() {
    document.getElementById('login-view').classList.add('hidden');
    document.getElementById('chat-view').classList.remove('hidden');
    document.getElementById('user-phone').textContent = state.phone;
}

function toggleSidebar() {
    document.getElementById('sidebar').classList.toggle('open');
}

function setLoading(loading) {
    document.getElementById('login-loading').classList.toggle('hidden', !loading);
    document.querySelectorAll('.login-card button').forEach(b => b.disabled = loading);
}

function showError(msg) {
    const el = document.getElementById('login-error');
    el.textContent = msg;
    el.classList.remove('hidden');
}

function hideError() {
    document.getElementById('login-error').classList.add('hidden');
}

// ─── API Helper ──────────────────────────────────────────────────────────────
async function api(url, opts = {}) {
    const fetchOpts = { method: opts.method || 'GET', headers: {} };
    if (opts.body) {
        fetchOpts.headers['Content-Type'] = 'application/json';
        fetchOpts.body = JSON.stringify(opts.body);
    }

    const resp = await fetch(url, fetchOpts);
    const data = await resp.json();

    if (!resp.ok) {
        throw new Error(data.error || `HTTP ${resp.status}`);
    }
    return data;
}

// ─── Reactions & Emoji Picker ────────────────────────────────────────────────
const QUICK_EMOJIS = ['👍', '❤️', '😂', '😮', '😢', '🙏'];
const ALL_EMOJIS = [
    '😀','😃','😄','😁','😆','😅','🤣','😂','🙂','😊','😇','🥰','😍','🤩',
    '😘','😗','😚','😋','😛','😜','🤪','😝','🤑','🤗','🤭','🤫','🤔','🤐',
    '🤨','😐','😑','😶','😏','😒','🙄','😬','🤥','😌','😔','😪','🤤','😴',
    '😷','🤒','🤕','🤢','🤮','🥵','🥶','🥴','😵','🤯','🤠','🥳','🥸','😎',
    '🤓','🧐','😕','😟','🙁','😮','😯','😲','😳','🥺','😦','😧','😨','😰',
    '😥','😢','😭','😱','😖','😣','😞','😓','😩','😫','🥱','😤','😡','😠',
    '🤬','👿','💀','💩','🤡','👹','👺','👻','👽','👾','🤖','😺','😸','😹',
    '😻','😼','😽','🙀','😿','😾','👋','🤚','🖐','✋','🖖','👌','🤌','🤏',
    '✌️','🤞','🤟','🤘','🤙','👈','👉','👆','👇','☝️','👍','👎','✊','👊',
    '🤛','🤜','👏','🙌','👐','🤲','🤝','🙏','💪','🦾','❤️','🧡','💛','💚',
    '💙','💜','🤎','🖤','🤍','💯','💢','💥','💫','💦','🔥','⭐','🌟','✨',
    '🎉','🎊','🏆','🏅','🥇','🥈','🥉','⚽','🎯','🎮','🎵','🎶','🔔',
];

function showReactionPicker(messageId) {
    closeAllPickers();
    const btn = event.target;
    const msgEl = btn.closest('.message');
    if (!msgEl) return;

    const picker = document.createElement('div');
    picker.className = 'reaction-picker';
    picker.innerHTML = QUICK_EMOJIS.map(e =>
        `<button class="reaction-emoji" onclick="sendReaction('${messageId}', '${e}')">${e}</button>`
    ).join('') + `<button class="reaction-emoji reaction-more" onclick="showFullEmojiPicker('${messageId}', this)">+</button>`;
    msgEl.appendChild(picker);
    autoClosePicker(picker);
}

function showFullEmojiPicker(messageId, btn) {
    closeAllPickers();
    const msgEl = btn.closest('.message');
    if (!msgEl) return;

    const picker = document.createElement('div');
    picker.className = 'reaction-picker reaction-picker-full';
    picker.innerHTML = ALL_EMOJIS.map(e =>
        `<button class="reaction-emoji" onclick="sendReaction('${messageId}', '${e}')">${e}</button>`
    ).join('');
    msgEl.appendChild(picker);
    autoClosePicker(picker);
}

// Emoji picker for the composer (insert emoji into message input)
function showComposerEmojiPicker() {
    closeAllPickers();
    const composer = document.querySelector('.composer');
    const picker = document.createElement('div');
    picker.className = 'reaction-picker reaction-picker-full composer-emoji-picker';
    picker.innerHTML = ALL_EMOJIS.map(e =>
        `<button class="reaction-emoji" onclick="insertComposerEmoji('${e}')">${e}</button>`
    ).join('');
    composer.appendChild(picker);
    autoClosePicker(picker);
}

function insertComposerEmoji(emoji) {
    closeAllPickers();
    const input = document.getElementById('message-input');
    const pos = input.selectionStart || input.value.length;
    input.value = input.value.slice(0, pos) + emoji + input.value.slice(pos);
    input.focus();
    input.selectionStart = input.selectionEnd = pos + emoji.length;
}

function closeAllPickers() {
    document.querySelectorAll('.reaction-picker').forEach(el => el.remove());
}

function autoClosePicker(picker) {
    // Delay adding the listener so the opening click doesn't immediately close it
    requestAnimationFrame(() => {
        requestAnimationFrame(() => {
            document.addEventListener('click', function close(ev) {
                if (!picker.contains(ev.target) && !ev.target.closest('.composer-btn')) {
                    picker.remove();
                    document.removeEventListener('click', close);
                }
            });
        });
    });
}

async function sendReaction(messageId, emoji) {
    closeAllPickers();

    const msg = state.messages.find(m => m.messageId === messageId);
    if (!msg) return;

    const to = getRecipientPhones(state.currentConversationId);
    if (to.length === 0) return;

    const targetBody = msg.messageBody || '';

    // Optimistic: show reaction badge immediately (surgical DOM update)
    if (!msg._reactions) msg._reactions = [];
    msg._reactions.push({ emoji, isMine: true });
    updateMessageReactions(messageId);

    try {
        await api('/api/messages/react', {
            method: 'POST',
            body: {
                conversationId: state.currentConversationId,
                to,
                emoji,
                targetBody,
            }
        });
        // No reloadCurrentConversation — SSE will deliver the reaction message
    } catch (e) {
        console.error('Failed to send reaction:', e);
        if (msg._reactions) {
            const idx = msg._reactions.findIndex(r => r.emoji === emoji && r.isMine);
            if (idx >= 0) msg._reactions.splice(idx, 1);
            updateMessageReactions(messageId);
        }
    }
}

// ─── Push Notifications ──────────────────────────────────────────────────────
async function requestNotificationPermission() {
    if (!('Notification' in window)) {
        alert('This browser does not support notifications.');
        return;
    }

    const current = Notification.permission;
    if (current === 'denied') {
        alert('Notifications are blocked. Please allow them in your browser settings (look for the lock icon in the address bar) and try again.');
        return;
    }

    if (current === 'default') {
        // Browser will show the permission prompt
        const result = await Notification.requestPermission();
        if (result === 'denied') {
            alert('Notifications were denied. You can change this in browser settings.');
            return;
        }
        if (result !== 'granted') return;
    }

    // Permission granted (either just now or previously) — (re-)register push
    await setupPushNotifications();
    alert('Push notifications ' + (current === 'default' ? 'enabled' : 're-registered') + '!');
}

async function setupPushNotifications() {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
        console.log('Push notifications not supported in this browser');
        return;
    }

    try {
        const registration = await navigator.serviceWorker.register('/sw.js');

        const resp = await api('/api/push/vapid-key');
        if (!resp.publicKey) return;

        let subscription = await registration.pushManager.getSubscription();

        if (!subscription) {
            subscription = await registration.pushManager.subscribe({
                userVisibleOnly: true,
                applicationServerKey: urlBase64ToUint8Array(resp.publicKey)
            });
        }

        await api('/api/push/subscribe', {
            method: 'POST',
            body: subscription.toJSON()
        });

        console.log('Push notifications enabled');
    } catch (e) {
        console.log('Push notification setup failed:', e.message);
    }
}

function urlBase64ToUint8Array(base64String) {
    var padding = '='.repeat((4 - base64String.length % 4) % 4);
    var base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    var rawData = window.atob(base64);
    var outputArray = new Uint8Array(rawData.length);
    for (var i = 0; i < rawData.length; ++i) {
        outputArray[i] = rawData.charCodeAt(i);
    }
    return outputArray;
}

// ─── ntfy Push Notifications ─────────────────────────────────────────────────
var ntfyInfo = null;

async function setupNtfyButton() {
    try {
        var resp = await api('/api/ntfy/info');
        var btn = document.getElementById('ntfy-btn');
        if (!btn) return;
        if (resp.enabled && resp.topic) {
            ntfyInfo = resp;
            btn.classList.remove('hidden');
            // Update button text based on subscription status
            btn.textContent = resp.subscribed ? '🔔 ntfy (on)' : '🔕 ntfy';
        } else {
            btn.classList.add('hidden');
        }
    } catch (e) {
        console.log('ntfy setup check failed:', e.message);
    }
}

async function enableNtfyOnServer() {
    if (ntfyInfo && ntfyInfo.subscribed) return;
    try {
        await api('/api/ntfy/subscribe', { method: 'POST', body: { enabled: true } });
        if (ntfyInfo) ntfyInfo.subscribed = true;
        var btn = document.getElementById('ntfy-btn');
        if (btn) btn.textContent = '🔔 ntfy (on)';
    } catch (e) {
        console.error('Failed to enable ntfy:', e);
    }
}

function openNtfySubscribe() {
    if (!ntfyInfo) return;

    var ua = navigator.userAgent || '';
    var isAndroid = /android/i.test(ua);
    var isIOS = /iP(hone|ad|od)/i.test(ua);

    // Enable server-side ntfy push when user subscribes
    enableNtfyOnServer();

    // Android: use ntfy:// deep link to open app directly
    if (isAndroid && ntfyInfo.appUrl) {
        hideNotificationMenu();
        window.location.href = ntfyInfo.appUrl;
        return;
    }

    // Desktop: open ntfy web interface for the topic
    if (!isIOS) {
        hideNotificationMenu();
        window.open(ntfyInfo.server + '/' + ntfyInfo.topic, '_blank');
        return;
    }

    // iOS: show topic info with copy button (no deep link support)
    hideNotificationMenu();

    var server = ntfyInfo.server === 'https://ntfy.sh' ? '' : ntfyInfo.server;
    var instructions = server
        ? 'Open the ntfy app, tap +, set server to <b>' + server + '</b> and paste the topic below.'
        : 'Open the ntfy app, tap + and paste the topic below.';

    var overlay = document.createElement('div');
    overlay.className = 'modal';
    overlay.onclick = function(e) { if (e.target === overlay) overlay.remove(); };
    overlay.innerHTML =
        '<div class="modal-content">' +
            '<h3>Subscribe via ntfy</h3>' +
            '<p style="font-size:0.9em;margin-bottom:12px">' + instructions + '</p>' +
            '<div style="display:flex;gap:8px;align-items:center">' +
                '<input type="text" readonly value="' + ntfyInfo.topic + '" style="flex:1;font-family:monospace;font-size:0.95em" id="ntfy-topic-input">' +
                '<button class="btn-primary" onclick="copyNtfyTopic()">Copy</button>' +
            '</div>' +
            '<p id="ntfy-copied" class="hidden" style="color:var(--accent);font-size:0.85em;margin-top:6px">Copied!</p>' +
        '</div>';
    document.body.appendChild(overlay);
}

function copyNtfyTopic() {
    var input = document.getElementById('ntfy-topic-input');
    if (!input) return;
    navigator.clipboard.writeText(input.value).then(function() {
        var msg = document.getElementById('ntfy-copied');
        if (msg) msg.classList.remove('hidden');
    });
}

// ─── Start ───────────────────────────────────────────────────────────────────
init();
