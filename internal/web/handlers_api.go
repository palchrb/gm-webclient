package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

func (s *Server) handleGetConversations(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	var opts []gm.GetConversationsOption
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil {
			opts = append(opts, gm.WithLimit(limit))
		}
	}

	result, err := session.API.GetConversations(r.Context(), opts...)
	if err != nil {
		handleAPIError(w, err, "get conversations")
		return
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

	result, err := session.API.SendMessage(r.Context(), req.To, req.Body)
	if err != nil {
		handleAPIError(w, err, "send message")
		return
	}
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

func handleAPIError(w http.ResponseWriter, err error, operation string) {
	if apiErr, ok := err.(*gm.APIError); ok {
		writeJSON(w, apiErr.StatusCode, map[string]string{"error": operation + ": " + apiErr.Body})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": operation + " failed"})
}
