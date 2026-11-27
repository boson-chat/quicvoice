// Package handlers holds the public HTTP API handlers.
package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/roles"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/voicechannel"
)

type RoomsHandler struct {
	Auth     auth.AuthServiceImpl
	Rooms    room.RoomServiceImpl
	Channels voicechannel.VoiceChannelServiceImpl
}

type roomsResponse struct {
	Permissions permissionsJSON `json:"permissions"`
	Rooms       []roomJSON      `json:"rooms"`
}

type permissionsJSON struct {
	// voice_channel_creation from the auth service.
	Policy      string `json:"policy"`
	CanCreate   bool   `json:"can_create"`
	CreateScope string `json:"create_scope"`
	// Only with create_scope=founded_channels (then always an array).
	FoundedChannels *[]string `json:"founded_channels,omitempty"`
	// The caller's effective network permissions (voice.manage_any,
	// voice.bypass_locks; auth admin on tokens without a perms claim).
	CanManageAny bool `json:"can_manage_any"`
	BypassLocks  bool `json:"bypass_locks"`
}

type ownerJSON struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
}

type roomJSON struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Kind     string     `json:"kind"` // static | dynamic | adhoc
	Parent   *string    `json:"parent"`
	Owner    *ownerJSON `json:"owner"`
	Position int        `json:"position"`
	// RFC 3339; null for ad-hoc rooms.
	CreatedAt                *time.Time `json:"created_at"`
	MinRole                  roles.Role `json:"min_role"`
	SpeakRole                roles.Role `json:"speak_role"`
	RequireChannelMembership bool       `json:"require_channel_membership"`
	// Network roles one of which is needed to join; always an array.
	AllowedRoles []string `json:"allowed_roles"`
	CanManage    bool     `json:"can_manage"`
	CanJoin      bool     `json:"can_join"`
	CanSpeak     bool     `json:"can_speak"`
	// Why the caller can't join; omitted when it can.
	LockReason   string            `json:"lock_reason,omitempty"`
	Participants []participantJSON `json:"participants"`
}

type participantJSON struct {
	SessionID uint16 `json:"session_id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Muted     bool   `json:"muted"`
	Deafened  bool   `json:"deafened"`
	Speaking  bool   `json:"speaking"`
	// Guest: an IRC guest with no account (name is an unverified nick).
	Guest bool `json:"guest,omitempty"`
}

type errorJSON struct {
	Error string `json:"error"`
	// Machine-readable: unauthorized, invalid, forbidden, static_room,
	// not_found, conflict, limit, unavailable, internal.
	Code string `json:"code,omitempty"`
}

func participantsJSON(ps []room.ParticipantInfo) []participantJSON {
	out := make([]participantJSON, 0, len(ps))
	for _, p := range ps {
		out = append(out, participantJSON{
			SessionID: p.SessionID, UserID: p.UserID, Name: p.Name,
			Muted: p.Muted, Deafened: p.Deafened, Speaking: p.Speaking, Guest: p.Guest,
		})
	}
	return out
}

func viewJSON(v voicechannel.View, ps []room.ParticipantInfo) roomJSON {
	r := v.Room
	j := roomJSON{
		ID: r.ID, Name: r.Name, Kind: v.Kind, Position: r.Position,
		MinRole: r.MinRole, SpeakRole: r.SpeakRole, RequireChannelMembership: r.RequireChannelMembership,
		AllowedRoles: append([]string{}, r.AllowedRoles...),
		CanManage:    v.CanManage, CanJoin: v.CanJoin, CanSpeak: v.CanSpeak, LockReason: v.LockReason,
		Participants: participantsJSON(ps),
	}
	if r.Parent != "" {
		p := r.Parent
		j.Parent = &p
	}
	if r.OwnerUserID != "" {
		j.Owner = &ownerJSON{UserID: r.OwnerUserID, Name: r.OwnerName}
	}
	if !r.CreatedAt.IsZero() {
		t := r.CreatedAt
		j.CreatedAt = &t
	}
	return j
}

// identify verifies the bearer voice token (exactly like the QUIC Hello).
func (h *RoomsHandler) identify(w nethttp.ResponseWriter, r *nethttp.Request) (*auth.Identity, bool) {
	w.Header().Set("Cache-Control", "no-store")
	token, ok := bearer(r)
	if !ok {
		unauthorized(w, "missing bearer token")
		return nil, false
	}
	id, err := h.Auth.Verify(r.Context(), token)
	if err != nil {
		msg := "invalid token"
		if errors.Is(err, auth.ErrTokenExpired) {
			msg = "token expired"
		}
		unauthorized(w, msg)
		return nil, false
	}
	return id, true
}

// List serves GET /v1/rooms. Rooms the token's `rooms` claim excludes are
// left out.
func (h *RoomsHandler) List(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.identify(w, r)
	if !ok {
		return
	}
	listing, err := h.Channels.List(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make(map[string]voicechannel.View, len(listing.Rooms))
	for _, v := range listing.Rooms {
		views[v.Room.ID] = v
	}
	// The room service gives the order (registry order, then live ad-hoc
	// rooms when not strict) and the live participants.
	infos := h.Rooms.List(r.Context(), id)
	resp := roomsResponse{
		Permissions: permissionsJSON{
			Policy: string(listing.Permissions.Policy), CanCreate: listing.Permissions.CanCreate,
			CreateScope:  string(listing.Permissions.Scope),
			CanManageAny: listing.Permissions.ManageAny, BypassLocks: listing.Permissions.BypassLocks,
		},
		Rooms: make([]roomJSON, 0, len(infos)),
	}
	if listing.Permissions.Scope == voicechannel.ScopeFounded {
		fc := append([]string{}, listing.Permissions.FoundedChannels...)
		resp.Permissions.FoundedChannels = &fc
	}
	for _, info := range infos {
		v, ok := views[info.ID]
		if !ok {
			// Ad-hoc room (VOICE_ROOMS_STRICT=false): no metadata, no locks.
			v = voicechannel.View{Room: voiceroom.Record{ID: info.ID, Name: info.Name},
				Kind: "adhoc", CanJoin: true, CanSpeak: id.CanSpeak()}
		}
		resp.Rooms = append(resp.Rooms, viewJSON(v, info.Participants))
	}
	if err := writeJSON(w, nethttp.StatusOK, resp); err != nil {
		logger.FromCtx(r.Context()).Debug().Err(err).Msg("write /v1/rooms")
	}
}

type createRequest struct {
	Name                     string      `json:"name"`
	Parent                   *string     `json:"parent"`
	MinRole                  *roles.Role `json:"min_role"`
	SpeakRole                *roles.Role `json:"speak_role"`
	RequireChannelMembership *bool       `json:"require_channel_membership"`
	AllowedRoles             []string    `json:"allowed_roles"`
}

// Create serves POST /v1/rooms → 201 with the room.
func (h *RoomsHandler) Create(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.identify(w, r)
	if !ok {
		return
	}
	var req createRequest
	if !decode(w, r, &req) {
		return
	}
	in := voicechannel.CreateInput{Name: req.Name, AllowedRoles: req.AllowedRoles}
	if req.Parent != nil {
		in.Parent = *req.Parent
	}
	if req.MinRole != nil {
		in.MinRole = *req.MinRole
	}
	if req.SpeakRole != nil {
		in.SpeakRole = *req.SpeakRole
	}
	if req.RequireChannelMembership != nil {
		in.RequireChannelMembership = *req.RequireChannelMembership
	}
	v, err := h.Channels.Create(r.Context(), id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/rooms/"+v.Room.ID)
	_ = writeJSON(w, nethttp.StatusCreated, viewJSON(v, nil))
}

type updateRequest struct {
	Name                     *string     `json:"name"`
	Position                 *int        `json:"position"`
	MinRole                  *roles.Role `json:"min_role"`
	SpeakRole                *roles.Role `json:"speak_role"`
	RequireChannelMembership *bool       `json:"require_channel_membership"`
	// Replaces the list; [] clears it, null/absent leaves it.
	AllowedRoles *[]string `json:"allowed_roles"`
	// Parent is immutable; sending it is rejected rather than ignored.
	Parent json.RawMessage `json:"parent"`
}

// Update serves PATCH /v1/rooms/{id} → 200 with the room.
func (h *RoomsHandler) Update(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.identify(w, r)
	if !ok {
		return
	}
	var req updateRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Parent != nil {
		writeError(w, nethttp.StatusBadRequest, "invalid", "parent can't be changed; create a new room")
		return
	}
	roomID := r.PathValue("id")
	v, err := h.Channels.Update(r.Context(), id, roomID, voicechannel.UpdateInput{
		Name: req.Name, Position: req.Position, MinRole: req.MinRole, SpeakRole: req.SpeakRole,
		RequireChannelMembership: req.RequireChannelMembership, AllowedRoles: req.AllowedRoles,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	_ = writeJSON(w, nethttp.StatusOK, viewJSON(v, h.Rooms.Participants(roomID)))
}

// Delete serves DELETE /v1/rooms/{id} → 204. Everyone in the room is
// removed with Error{KICKED, "voice channel deleted"}.
func (h *RoomsHandler) Delete(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.identify(w, r)
	if !ok {
		return
	}
	rec, err := h.Channels.Delete(r.Context(), id, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n := h.Rooms.CloseRoom(r.Context(), rec.ID, fmt.Sprintf("voice channel %q was deleted by %s", rec.Name, id.DisplayName))
	logger.FromCtx(r.Context()).Info().Str("room", rec.ID).Int("kicked", n).Msg("voice room closed")
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *RoomsHandler) fail(w nethttp.ResponseWriter, r *nethttp.Request, err error) {
	status, code := nethttp.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, voicechannel.ErrInvalid):
		status, code = nethttp.StatusBadRequest, "invalid"
	case errors.Is(err, voicechannel.ErrStatic):
		status, code = nethttp.StatusForbidden, "static_room"
	case errors.Is(err, voicechannel.ErrForbidden):
		status, code = nethttp.StatusForbidden, "forbidden"
	case errors.Is(err, voicechannel.ErrNotFound):
		status, code = nethttp.StatusNotFound, "not_found"
	case errors.Is(err, voicechannel.ErrConflict):
		status, code = nethttp.StatusConflict, "conflict"
	case errors.Is(err, voicechannel.ErrLimit):
		status, code = nethttp.StatusForbidden, "limit"
	case errors.Is(err, voicechannel.ErrUnavailable):
		status, code = nethttp.StatusServiceUnavailable, "unavailable"
		w.Header().Set("Retry-After", "5")
	}
	msg := err.Error()
	if status == nethttp.StatusInternalServerError {
		logger.FromCtx(r.Context()).Error().Err(err).Str("path", r.URL.Path).Msg("rooms API")
		msg = "internal error"
	}
	writeError(w, status, code, msg)
}

const maxBody = 4 << 10

func decode(w nethttp.ResponseWriter, r *nethttp.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeError(w, nethttp.StatusUnsupportedMediaType, "invalid", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody+1))
	if err := dec.Decode(v); err != nil {
		writeError(w, nethttp.StatusBadRequest, "invalid", "bad JSON body: "+err.Error())
		return false
	}
	return true
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
	writeError(w, nethttp.StatusUnauthorized, "unauthorized", msg)
}

func writeError(w nethttp.ResponseWriter, status int, code, msg string) {
	_ = writeJSON(w, status, errorJSON{Error: msg, Code: code})
}

func writeJSON(w nethttp.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}
