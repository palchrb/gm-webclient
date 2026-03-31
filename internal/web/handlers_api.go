package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

func (s *Server) handleGetConversations(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	var opts []gm.GetConversationsOption

	limit := 200
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			limit = l
		}
	}
	opts = append(opts, gm.WithLimit(limit))

	if beforeStr := r.URL.Query().Get("beforeDate"); beforeStr != "" {
		if t, err := time.Parse(time.RFC3339, beforeStr); err == nil {
			opts = append(opts, gm.WithAfterDate(time.Time{}))
			// The Garmin API uses AfterDate for filtering, but to paginate
			// backwards we fetch all and filter server-side below.
			_ = t
		}
	}

	result, err := session.API.GetConversations(r.Context(), opts...)
	if err != nil {
		handleAPIError(w, err, "get conversations")
		return
	}

	// Server-side filtering for beforeDate pagination
	if beforeStr := r.URL.Query().Get("beforeDate"); beforeStr != "" {
		if before, err := time.Parse(time.RFC3339, beforeStr); err == nil {
			filtered := make([]gm.ConversationMetaModel, 0, len(result.Conversations))
			for _, c := range result.Conversations {
				if c.UpdatedDate.Before(before) {
					filtered = append(filtered, c)
				}
			}
			result.Conversations = filtered
		}
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetConversationDetail(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation ID"})
		return
	}

	var opts []gm.GetConversationDetailOption
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil {
			opts = append(opts, gm.WithDetailLimit(limit))
		}
	}
	if olderStr := r.URL.Query().Get("olderThanId"); olderStr != "" {
		if olderID, err := uuid.Parse(olderStr); err == nil {
			opts = append(opts, gm.WithOlderThanID(olderID))
		}
	}
	if newerStr := r.URL.Query().Get("newerThanId"); newerStr != "" {
		if newerID, err := uuid.Parse(newerStr); err == nil {
			opts = append(opts, gm.WithNewerThanID(newerID))
		}
	}

	result, err := session.API.GetConversationDetail(r.Context(), convID, opts...)
	if err != nil {
		handleAPIError(w, err, "get conversation detail")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetConversationMembers(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation ID"})
		return
	}

	result, err := session.API.GetConversationMembers(r.Context(), convID)
	if err != nil {
		handleAPIError(w, err, "get conversation members")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type sendMessageRequest struct {
	ConversationID string   `json:"conversationId"`
	To             []string `json:"to"`
	Body           string   `json:"body"`
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	if len(req.To) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to is required"})
		return
	}

	s.logger.Debug("Sending message",
		"to", req.To,
		"conversationId", req.ConversationID,
		"bodyLen", len(req.Body),
	)

	result, err := session.API.SendMessage(r.Context(), req.To, req.Body)
	if err != nil {
		handleAPIError(w, err, "send message")
		return
	}

	s.logger.Debug("Message sent",
		"messageId", result.MessageID,
		"conversationId", result.ConversationID,
	)

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMarkAsRead(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	convID, err := uuid.Parse(r.PathValue("convId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation ID"})
		return
	}
	msgID, err := uuid.Parse(r.PathValue("msgId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid message ID"})
		return
	}

	result, err := session.API.MarkAsRead(r.Context(), convID, msgID)
	if err != nil {
		handleAPIError(w, err, "mark as read")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetMediaURL(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	msgUUID, err := uuid.Parse(r.URL.Query().Get("uuid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid uuid"})
		return
	}
	mediaID, err := uuid.Parse(r.URL.Query().Get("mediaId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid mediaId"})
		return
	}
	messageID, err := uuid.Parse(r.URL.Query().Get("messageId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid messageId"})
		return
	}
	conversationID, err := uuid.Parse(r.URL.Query().Get("conversationId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversationId"})
		return
	}
	mediaType := gm.MediaType(r.URL.Query().Get("mediaType"))

	result, err := session.API.GetMediaDownloadURL(r.Context(), msgUUID, mediaID, messageID, conversationID, mediaType)
	if err != nil {
		handleAPIError(w, err, "get media URL")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleLeaveConversation(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation ID"})
		return
	}

	s.logger.Debug("Leaving conversation", "conversationId", convID)

	if err := session.API.LeaveConversation(r.Context(), convID); err != nil {
		handleAPIError(w, err, "leave conversation")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "left"})
}

func (s *Server) handleNewChat(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	var req struct {
		Phone string `json:"phone"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Phone == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone and body are required"})
		return
	}

	// Derive the Hermes user ID from the phone number
	recipientID := gm.PhoneToHermesUserID(req.Phone)
	result, err := session.API.SendMessage(r.Context(), []string{recipientID}, req.Body)
	if err != nil {
		handleAPIError(w, err, "send new chat message")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func handleAPIError(w http.ResponseWriter, err error, operation string) {
	if apiErr, ok := err.(*gm.APIError); ok {
		writeJSON(w, apiErr.StatusCode, map[string]string{"error": operation + ": " + apiErr.Body})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": operation + " failed"})
}
