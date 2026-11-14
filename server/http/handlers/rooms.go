// Package handlers holds the public HTTP API handlers.
package handlers

import (
	"encoding/json"
	"errors"
	nethttp "net/http"
	"strings"

	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
)

type RoomsHandler struct {
	Auth  auth.AuthServiceImpl
	Rooms room.RoomServiceImpl
}

type roomsResponse struct {
	Rooms []roomJSON `json:"rooms"`
}

type roomJSON struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Participants []participantJSON `json:"participants"`
}

type participantJSON struct {
	SessionID uint16 `json:"session_id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Muted     bool   `json:"muted"`
	Deafened  bool   `json:"deafened"`
	Speaking  bool   `json:"speaking"`
}

type errorJSON struct {
	Error string `json:"error"`
}

// List serves GET /v1/rooms. The bearer token is a voice token, verified
// exactly like the QUIC Hello; rooms its `rooms` claim excludes are left out.
func (h *RoomsHandler) List(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Cache-Control", "no-store")
	token, ok := bearer(r)
	if !ok {
		unauthorized(w, "missing bearer token")
		return
	}
	id, err := h.Auth.Verify(r.Context(), token)
	if err != nil {
		msg := "invalid token"
		if errors.Is(err, auth.ErrTokenExpired) {
			msg = "token expired"
		}
		unauthorized(w, msg)
		return
	}

	rooms := h.Rooms.List(r.Context(), id)
	resp := roomsResponse{Rooms: make([]roomJSON, 0, len(rooms))}
	for _, rm := range rooms {
		ps := make([]participantJSON, 0, len(rm.Participants))
		for _, p := range rm.Participants {
			ps = append(ps, participantJSON{
				SessionID: p.SessionID, UserID: p.UserID, Name: p.Name,
				Muted: p.Muted, Deafened: p.Deafened, Speaking: p.Speaking,
			})
		}
		resp.Rooms = append(resp.Rooms, roomJSON{ID: rm.ID, Name: rm.Name, Participants: ps})
	}
	if err := writeJSON(w, nethttp.StatusOK, resp); err != nil {
		logger.FromCtx(r.Context()).Debug().Err(err).Msg("write /v1/rooms")
	}
}

func bearer(r *nethttp.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(w nethttp.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="quicvoice"`)
	_ = writeJSON(w, nethttp.StatusUnauthorized, errorJSON{Error: msg})
}

func writeJSON(w nethttp.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}
