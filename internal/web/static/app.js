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
            loadConversations();
            connectSSE();
            setupPushNotifications();
            setupClipboardPaste();
        }
    } catch (e) {
        // Not logged in, show login view
    }
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
async function requestOTP() {
    const phone = document.getElementById('phone-input').value.trim();
    if (!phone) return;

    setLoading(true);
    hideError();

    try {
        await api('/api/auth/request-otp', { method: 'POST', body: { phone } });
        document.getElementById('phone-step').classList.add('hidden');
        document.getElementById('otp-step').classList.remove('hidden');
        document.getElementById('otp-phone').textContent = phone;
        document.getElementById('otp-input').focus();
    } catch (e) {
        showError(e.message || 'Could not send code');
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
        state.loggedIn = true;
        state.phone = resp.phone;
        state.userId = resp.userId;
        showChatView();
        loadConversations();
        connectSSE();
        setupPushNotifications();
    } catch (e) {
        showError(e.message || 'Invalid code');
    } finally {
        setLoading(false);
    }
}

async function logout() {
    try { await api('/api/auth/logout', { method: 'POST' }); } catch (e) { /* ignore */ }
    if (state.eventSource) state.eventSource.close();
    cache.clear();
    state.loggedIn = false;
    state.phone = null;
    state.conversations = [];
    state.currentConversationId = null;
    state.messages = [];
    location.reload();
}

// ─── Conversations ───────────────────────────────────────────────────────────
async function loadConversations() {
    // Show cached data immediately (avoids blank screen + reduces API calls).
    // Cache is long-lived (24h) — real-time updates come via SSE/SignalR,
    // and we do one background refresh per page load to catch anything missed.
    const cached = cache.get('conversations'); // no TTL — show cached instantly, always refresh below
    const cachedMembers = cache.get('members');
    const cachedNames = cache.get('memberNames');

    if (cached) {
        state.conversations = cached;
        if (cachedMembers) state.members = cachedMembers;
        if (cachedNames) state.memberNames = cachedNames;
        renderConversations();
    }

    // One background refresh per page load to catch anything missed while offline
    try {
        const resp = await api('/api/conversations?limit=500');
        state.conversations = (resp.conversations || []).sort(
            (a, b) => new Date(b.updatedDate) - new Date(a.updatedDate)
        );
        cache.set('conversations', state.conversations);
        renderConversations();
        // Only fetch members we don't already have cached
        for (const conv of state.conversations) {
            loadMembers(conv.conversationId);
        }
    } catch (e) {
        console.error('Failed to load conversations:', e);
    }
}

async function loadMembers(convId) {
    if (state.members[convId]) return;
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

    document.getElementById('no-conversation').classList.add('hidden');
    document.getElementById('conversation-view').classList.remove('hidden');

    // Close sidebar on mobile
    document.getElementById('sidebar').classList.remove('open');

    // Show cached messages immediately, then refresh from API
    const cachedMsgs = cache.get('msgs_' + convId); // no TTL — show cached instantly, always refresh below
    if (cachedMsgs) {
        state.messages = cachedMsgs;
        renderMessages();
        scrollToBottom();
    }

    try {
        const resp = await api(`/api/conversations/${convId}?limit=50`);
        state.messages = (resp.messages || []).sort(
            (a, b) => new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0)
        );
        cache.set('msgs_' + convId, state.messages);
        renderMessages();
        scrollToBottom();

        // Mark last message as read
        if (state.messages.length > 0) {
            const lastMsg = state.messages[state.messages.length - 1];
            markAsRead(convId, lastMsg.messageId);
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
    input.focus();

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
    state.messages.push({
        messageId: messageId,
        conversationId: convId,
        from: state.userId,
        messageBody: body,
        sentAt: new Date().toISOString(),
        status: [],
        _sendState: sendState || 'sent',
    });
    renderMessages();
    scrollToBottom();
}

// Replace a temporary optimistic message with the real server ID.
function replaceOptimisticMessage(convId, tempId, realId, sendState) {
    const msg = state.messages.find(m => m.messageId === tempId);
    if (msg) {
        msg.messageId = realId;
        msg._sendState = sendState || 'sent';
        renderMessages();
    }
}

// Mark an optimistic message as failed.
function markOptimisticFailed(tempId, errorMsg) {
    const msg = state.messages.find(m => m.messageId === tempId);
    if (msg) {
        msg._sendState = 'failed';
        msg._errorMsg = errorMsg;
        renderMessages();
    }
}

// Reload the current conversation messages from the server.
async function reloadCurrentConversation() {
    const convId = state.currentConversationId;
    if (!convId) return;
    try {
        const resp = await api(`/api/conversations/${convId}?limit=50`);
        state.messages = (resp.messages || []).sort(
            (a, b) => new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0)
        );
        renderMessages();
        scrollToBottom();
    } catch (e) {
        console.error('Failed to reload conversation:', e);
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
    if (!msg.mediaId) return null;
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
        previewAudio.textContent = 'Audio: ' + file.name;
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
        // Reload conversation to get the full message with media URLs
        reloadCurrentConversation();
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
        // Prefer webm/opus which ffmpeg can easily convert to OGG
        const mimeType = MediaRecorder.isTypeSupported('audio/webm;codecs=opus')
            ? 'audio/webm;codecs=opus'
            : 'audio/webm';
        mediaRecorder = new MediaRecorder(stream, { mimeType });
        audioChunks = [];

        mediaRecorder.ondataavailable = (e) => {
            if (e.data.size > 0) audioChunks.push(e.data);
        };

        mediaRecorder.onstop = () => {
            stream.getTracks().forEach(t => t.stop());
            clearInterval(recordingTimer);
            const blob = new Blob(audioChunks, { type: mimeType });
            const file = new File([blob], 'voice.webm', { type: mimeType });
            sendMediaFile(file);
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

    // Reconnect and catch up on tab focus
    document.addEventListener('visibilitychange', () => {
        if (!document.hidden && state.loggedIn) {
            loadConversations();
            if (state.currentConversationId) {
                selectConversation(state.currentConversationId);
            }
        }
    });
}

function handleIncomingMessage(msg) {
    const convId = msg.conversationId;

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

    // If viewing this conversation, append message
    if (state.currentConversationId === convId) {
        // Avoid duplicates
        if (!state.messages.find(m => m.messageId === msg.messageId)) {
            state.messages.push(msg);
            // Update message cache with live data
            cache.set('msgs_' + convId, state.messages);
            renderMessages();
            scrollToBottom();
            markAsRead(convId, msg.messageId);
        }
    }
}

function handleStatusUpdate(update) {
    // Update message status in current view
    if (!update.messageId) return;
    const msgId = update.messageId.messageId;
    const msg = state.messages.find(m => m.messageId === msgId);
    if (msg && update.messageStatus) {
        // Update the status array
        if (!msg.status) msg.status = [];
        const existing = msg.status.find(s => s.userId === (update.userId || ''));
        if (existing) {
            existing.messageStatus = update.messageStatus;
        } else {
            msg.status.push({ userId: update.userId || '', messageStatus: update.messageStatus });
        }
        renderMessages();
    }
}

// ─── Rendering ───────────────────────────────────────────────────────────────
function renderConversations() {
    const list = document.getElementById('conversation-list');
    if (state.conversations.length === 0) {
        list.innerHTML = '<div class="empty-state" style="height:200px">No conversations</div>';
        return;
    }

    list.innerHTML = state.conversations.map(conv => {
        const active = conv.conversationId === state.currentConversationId ? 'active' : '';
        const name = getConversationName(conv);
        const initial = name.charAt(0).toUpperCase();
        const time = formatTime(conv.updatedDate);

        return `
            <div class="conversation-item ${active}" onclick="selectConversation('${conv.conversationId}')">
                <div class="conversation-avatar">${initial}</div>
                <div class="conversation-info">
                    <div class="conversation-name">${escapeHtml(name)}</div>
                </div>
                <div class="conversation-time">${time}</div>
            </div>
        `;
    }).join('');
}

// Detect Garmin ZWS-encoded reaction: \u200b{emoji}\u200b to \u200a{text}\u200a
function isReactionMessage(msg) {
    const body = msg.messageBody || '';
    return body.startsWith('\u200b');
}

function extractReaction(msg) {
    const body = msg.messageBody || '';
    const match = body.match(/^\u200b(.+?)\u200b to \u200a(.*?)\u200a$/);
    if (!match) return null;
    return { emoji: match[1], targetText: match[2] };
}

function renderMessages() {
    const container = document.getElementById('messages');
    if (state.messages.length === 0) {
        container.innerHTML = '<div class="empty-state">No messages</div>';
        return;
    }

    // First pass: collect reactions and map them to target messages.
    // Garmin matches reactions by quoted body text to the most recent message.
    const reactions = {}; // messageId -> [{emoji, from}]
    const reactionMsgIds = new Set();

    for (const msg of state.messages) {
        if (!isReactionMessage(msg)) continue;
        const r = extractReaction(msg);
        if (!r) continue;
        reactionMsgIds.add(msg.messageId);
        // Find target: most recent message whose body matches r.targetText
        let target = null;
        for (let i = state.messages.length - 1; i >= 0; i--) {
            const candidate = state.messages[i];
            if (candidate.messageId === msg.messageId) continue;
            const candidateBody = (candidate.messageBody || '').replace(/[\u200a\u200b\u2009]/g, '').trim();
            if (candidateBody === r.targetText) {
                target = candidate;
                break;
            }
        }
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
    let lastDate = '';

    for (const msg of state.messages) {
        if (reactionMsgIds.has(msg.messageId)) continue; // skip reaction messages

        const date = formatDate(msg.sentAt || msg.receivedAt);
        if (date !== lastDate) {
            html += `<div class="message-time-separator">${date}</div>`;
            lastDate = date;
        }

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
        const mediaHtml = getMediaPlaceholder(msg);
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

        // Render reaction badges
        const msgReactions = reactions[msg.messageId] || [];
        let reactionsHtml = '';
        if (msgReactions.length > 0) {
            // Group by emoji and count
            const counts = {};
            for (const r of msgReactions) {
                counts[r.emoji] = (counts[r.emoji] || 0) + 1;
            }
            const badges = Object.entries(counts).map(([emoji, count]) =>
                `<span class="reaction-badge">${emoji}${count > 1 ? ' ' + count : ''}</span>`
            ).join('');
            reactionsHtml = `<div class="reaction-badges">${badges}</div>`;
        }

        html += `
            <div class="message ${cls}${failedCls}${sendingCls}">
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
            </div>
        `;
    }

    container.innerHTML = html;

    // Load media asynchronously after rendering
    loadMediaForMessages();
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
    // Try exact match, then lowercase (UUIDs from API may differ in case)
    return state.memberNames[from]
        || state.memberNames[from.toLowerCase()]
        || from;
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

function getMediaPlaceholder(msg) {
    if (!msg.mediaId) return '';
    const msgId = msg.messageId;
    if (msg.mediaType === 'ImageAvif') {
        return `<div class="message-image-container" id="media-${msgId}"><span style="color:var(--text-muted);font-size:12px">Loading image...</span></div>`;
    }
    if (msg.mediaType === 'AudioOgg') {
        return `<div class="message-audio" id="media-${msgId}"><span style="color:var(--text-muted);font-size:12px">Loading audio...</span></div>`;
    }
    return '';
}

function loadMediaForMessages() {
    const convId = state.currentConversationId;
    if (!convId) return;

    for (const msg of state.messages) {
        if (!msg.mediaId) continue;
        const el = document.getElementById(`media-${msg.messageId}`);
        if (!el || el.dataset.loaded) continue;
        el.dataset.loaded = 'true';

        const url = getMediaProxyUrl(msg, convId);
        if (!url) continue;

        if (msg.mediaType === 'ImageAvif') {
            el.innerHTML = `<img class="message-image" src="${escapeHtml(url)}" alt="Image" onclick="window.open('${escapeHtml(url)}', '_blank')" loading="lazy">`;
        } else if (msg.mediaType === 'AudioOgg') {
            createWaveformPlayer(el, url);
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
    audio.preload = 'metadata';
    audio.src = audioUrl;

    const barCount = 40;
    let bars = [];
    let isPlaying = false;
    let waveformGenerated = false;

    // Create placeholder bars
    for (let i = 0; i < barCount; i++) {
        const bar = document.createElement('div');
        bar.className = 'waveform-bar';
        bar.style.height = (20 + Math.random() * 60) + '%';
        barsContainer.appendChild(bar);
        bars.push(bar);
    }

    // Generate real waveform from audio data
    async function generateWaveform() {
        if (waveformGenerated) return;
        waveformGenerated = true;
        try {
            const resp = await fetch(audioUrl);
            const arrayBuf = await resp.arrayBuffer();
            const audioCtx = new (window.AudioContext || window.webkitAudioContext)();
            const decoded = await audioCtx.decodeAudioData(arrayBuf);
            const channelData = decoded.getChannelData(0);
            audioCtx.close();

            const samplesPerBar = Math.floor(channelData.length / barCount);
            const amplitudes = [];
            let maxAmp = 0;
            for (let i = 0; i < barCount; i++) {
                let sum = 0;
                const start = i * samplesPerBar;
                for (let j = start; j < start + samplesPerBar && j < channelData.length; j++) {
                    sum += Math.abs(channelData[j]);
                }
                const avg = sum / samplesPerBar;
                amplitudes.push(avg);
                if (avg > maxAmp) maxAmp = avg;
            }
            // Normalize and set bar heights (min 10%, max 100%)
            for (let i = 0; i < barCount; i++) {
                const normalized = maxAmp > 0 ? amplitudes[i] / maxAmp : 0;
                bars[i].style.height = (10 + normalized * 90) + '%';
            }
        } catch (e) {
            // Keep placeholder bars on error
        }
    }

    playBtn.onclick = () => {
        if (isPlaying) {
            audio.pause();
        } else {
            // Pause any other playing audio
            document.querySelectorAll('.waveform-player.playing').forEach(p => {
                if (p !== player) {
                    const otherBtn = p.querySelector('.waveform-play-btn');
                    if (otherBtn) otherBtn.click();
                }
            });
            audio.play();
            generateWaveform();
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
        const activeCount = Math.floor(progress * barCount);
        for (let i = 0; i < barCount; i++) {
            bars[i].classList.toggle('active', i < activeCount);
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

function escapeHtml(str) {
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

function scrollToBottom() {
    const container = document.getElementById('messages');
    requestAnimationFrame(() => {
        container.scrollTop = container.scrollHeight;
    });
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
    setTimeout(() => document.addEventListener('click', function close(ev) {
        if (!picker.contains(ev.target)) { picker.remove(); document.removeEventListener('click', close); }
    }), 0);
}

async function sendReaction(messageId, emoji) {
    document.querySelectorAll('.reaction-picker').forEach(el => el.remove());

    const msg = state.messages.find(m => m.messageId === messageId);
    if (!msg) return;

    const to = getRecipientPhones(state.currentConversationId);
    if (to.length === 0) return;

    const targetBody = msg.messageBody || '';

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
    } catch (e) {
        console.error('Failed to send reaction:', e);
    }
}

// ─── Push Notifications ──────────────────────────────────────────────────────
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

// ─── Start ───────────────────────────────────────────────────────────────────
init();
