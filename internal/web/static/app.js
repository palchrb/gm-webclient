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
        }
    } catch (e) {
        // Not logged in, show login view
    }
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
    state.loggedIn = false;
    state.phone = null;
    state.conversations = [];
    state.currentConversationId = null;
    state.messages = [];
    location.reload();
}

// ─── Conversations ───────────────────────────────────────────────────────────
async function loadConversations() {
    try {
        // Fetch all conversations by paginating until no more are returned.
        // The Garmin API returns up to `limit` per request; we keep fetching
        // older pages until we get fewer than the page size.
        let allConversations = [];
        let oldestDate = null;
        const pageSize = 200;

        while (true) {
            let url = `/api/conversations?limit=${pageSize}`;
            if (oldestDate) {
                // Fetch conversations updated before the oldest we've seen
                url += `&beforeDate=${encodeURIComponent(oldestDate)}`;
            }
            const resp = await api(url);
            const page = resp.conversations || [];
            if (page.length === 0) break;

            allConversations = allConversations.concat(page);

            // Find the oldest updatedDate in this page for next pagination
            const dates = page.map(c => new Date(c.updatedDate).getTime()).filter(d => !isNaN(d));
            if (dates.length > 0) {
                const minDate = new Date(Math.min(...dates));
                oldestDate = minDate.toISOString();
            }

            // If we got fewer than the page size, we've reached the end
            if (page.length < pageSize) break;
        }

        // Deduplicate by conversationId (in case of overlap between pages)
        const seen = new Set();
        state.conversations = [];
        for (const conv of allConversations) {
            if (!seen.has(conv.conversationId)) {
                seen.add(conv.conversationId);
                state.conversations.push(conv);
            }
        }
        state.conversations.sort((a, b) => new Date(b.updatedDate) - new Date(a.updatedDate));

        // Load members for each conversation
        for (const conv of state.conversations) {
            loadMembers(conv.conversationId);
        }
        renderConversations();
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

    // Load messages
    try {
        const resp = await api(`/api/conversations/${convId}?limit=50`);
        state.messages = (resp.messages || []).sort(
            (a, b) => new Date(a.sentAt || a.receivedAt || 0) - new Date(b.sentAt || b.receivedAt || 0)
        );
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
    if (!body || !state.currentConversationId) return;

    // Use phone numbers (member.address) for recipients, not Hermes UUIDs.
    // The Garmin API matches conversations by phone number; sending UUIDs
    // creates a new conversation instead of using the existing one.
    const to = getRecipientPhones(state.currentConversationId);
    if (to.length === 0) {
        console.error('No recipient phone numbers found — members may not be loaded yet');
        return;
    }

    const convId = state.currentConversationId;
    input.value = '';
    input.focus();

    try {
        const result = await api('/api/messages/send', {
            method: 'POST',
            body: { conversationId: convId, to, body }
        });
        // Optimistically add sent message to UI immediately
        addOptimisticMessage(convId, result.messageId, body);
    } catch (e) {
        console.error('Failed to send message:', e);
        input.value = body;
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
function addOptimisticMessage(convId, messageId, body) {
    if (state.currentConversationId !== convId) return;
    // Avoid duplicate if SSE already delivered it
    if (state.messages.find(m => m.messageId === messageId)) return;
    state.messages.push({
        messageId: messageId,
        conversationId: convId,
        from: state.userId,
        messageBody: body,
        sentAt: new Date().toISOString(),
        status: [],
    });
    renderMessages();
    scrollToBottom();
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
    if (!msg.mediaId || !msg.uuid) return null;
    const params = new URLSearchParams({
        uuid: msg.uuid,
        mediaId: msg.mediaId,
        messageId: msg.messageId,
        conversationId: convId,
        mediaType: msg.mediaType || 'ImageAvif',
    });
    return `/api/media/proxy?${params}`;
}

async function sendMediaFile(file) {
    if (!state.currentConversationId) return;
    const convId = state.currentConversationId;
    const to = getRecipientPhones(convId);
    if (to.length === 0) {
        console.error('No recipient phone numbers found');
        return;
    }

    // Show optimistic "sending..." message
    const tempId = 'temp-' + Date.now();
    addOptimisticMessage(convId, tempId, file.type.startsWith('image/') ? '(sending image...)' : '(sending audio...)');

    const form = new FormData();
    form.append('file', file);
    form.append('to', JSON.stringify(to));
    form.append('conversationId', convId);

    try {
        const resp = await fetch('/api/media/send', { method: 'POST', body: form });
        const data = await resp.json();
        if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
        // Reload conversation to get the full message with media details
        reloadCurrentConversation();
    } catch (e) {
        console.error('Failed to send media:', e);
        alert('Failed to send media: ' + e.message);
        // Remove optimistic message on failure
        state.messages = state.messages.filter(m => m.messageId !== tempId);
        renderMessages();
    }
}

function handleFileSelect() {
    const input = document.getElementById('file-input');
    if (input.files.length > 0) {
        sendMediaFile(input.files[0]);
        input.value = '';
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
        console.error('Microphone access denied:', e);
        alert('Microphone access is required for voice messages');
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
    renderConversations();

    // If viewing this conversation, append message
    if (state.currentConversationId === convId) {
        // Avoid duplicates
        if (!state.messages.find(m => m.messageId === msg.messageId)) {
            state.messages.push(msg);
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

function renderMessages() {
    const container = document.getElementById('messages');
    if (state.messages.length === 0) {
        container.innerHTML = '<div class="empty-state">No messages</div>';
        return;
    }

    let html = '';
    let lastDate = '';

    for (const msg of state.messages) {
        const date = formatDate(msg.sentAt || msg.receivedAt);
        if (date !== lastDate) {
            html += `<div class="message-time-separator">${date}</div>`;
            lastDate = date;
        }

        const isSent = isMine(msg);
        const cls = isSent ? 'sent' : 'received';
        const senderName = !isSent ? getSenderName(msg) : null;
        const body = getMessageBody(msg);
        const time = formatMessageTime(msg.sentAt || msg.receivedAt);
        const statusIcon = isSent ? getStatusIcon(msg) : '';
        const location = getLocationHtml(msg);
        const device = getDeviceLabel(msg);
        const mediaHtml = getMediaPlaceholder(msg);
        const transcription = msg.transcription
            ? `<div class="message-transcription">${escapeHtml(msg.transcription)}</div>` : '';

        html += `
            <div class="message ${cls}">
                ${senderName ? `<div class="message-sender">${escapeHtml(senderName)}</div>` : ''}
                <div class="message-bubble">${escapeHtml(body)}${mediaHtml}${location}${transcription}</div>
                <div class="message-meta">
                    <span>${time}</span>
                    ${device ? `<span class="message-device">${device}</span>` : ''}
                    ${statusIcon ? `<span class="message-status ${getStatusClass(msg)}">${statusIcon}</span>` : ''}
                </div>
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

    // Fallback: use member IDs, lookup name by lowercase UUID
    const otherIds = conv.memberIds.filter(id => id.toLowerCase() !== state.userId);
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
        body = (body ? body + ' ' : '') + '🎤 ' + msg.transcription;
    }
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
        if (!msg.mediaId || !msg.uuid) continue;
        const el = document.getElementById(`media-${msg.messageId}`);
        if (!el || el.dataset.loaded) continue;
        el.dataset.loaded = 'true';

        const url = getMediaProxyUrl(msg, convId);
        if (!url) continue;

        if (msg.mediaType === 'ImageAvif') {
            el.innerHTML = `<img class="message-image" src="${escapeHtml(url)}" alt="Image" onclick="window.open('${escapeHtml(url)}', '_blank')" loading="lazy">`;
        } else if (msg.mediaType === 'AudioOgg') {
            el.innerHTML = `<audio controls preload="none"><source src="${escapeHtml(url)}" type="audio/ogg">Your browser does not support audio.</audio>`;
        }
    }
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
