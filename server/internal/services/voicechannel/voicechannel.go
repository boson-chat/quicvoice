// Package voicechannel is the voice room registry: static rooms from
// VOICE_ROOMS plus rooms users create through the public API, optionally
// under an IRC channel ("parent"), with join/speak locks on IRC channel
// roles.
//
// Who may create rooms is the auth service's voice_channel_creation
// setting (admins | founders | registered), read through the settings
// service. IRC roles (founder, channel modes) come from Ergo through the
// ergo service.
//
// Network permissions come from the voice token. Tokens with the `perms`
// claim are judged by it alone: voice.manage_any (manage every dynamic
// room, create under any policy, no per-user limit, set oper locks),
// voice.bypass_locks (pass every join/speak lock) and voice.create (needed
// to create under the founders/registered policies). Older tokens without
// `perms` keep the legacy rule: auth admins (voice role moderator/admin)
// pass every check. Rooms can also be limited to network roles (the token's
// `roles` claim) with allowed_roles.
//
// It implements room.Directory (so strict mode only admits registry rooms)
// and room.JoinPolicy (role locks at Join).
package voicechannel

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/boson-chat/quicvoice/server/internal/db/repositories/voiceroom"
	"github.com/boson-chat/quicvoice/server/internal/logger"
	"github.com/boson-chat/quicvoice/server/internal/roles"
	"github.com/boson-chat/quicvoice/server/internal/services/auth"
	"github.com/boson-chat/quicvoice/server/internal/services/ergo"
	"github.com/boson-chat/quicvoice/server/internal/services/room"
	"github.com/boson-chat/quicvoice/server/internal/services/settings"
	"github.com/boson-chat/quicvoice/server/metrics"
)

var (
	ErrInvalid   = errors.New("invalid request")
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("no such room")
	ErrConflict  = errors.New("a room with that name already exists in that channel")
	ErrLimit     = errors.New("room limit reached")
	ErrStatic    = errors.New("configured room (VOICE_ROOMS) can't be changed through the API")
	// ErrUnavailable: an IRC role check was needed but Ergo couldn't be asked.
	ErrUnavailable = errors.New("IRC role check unavailable, try again shortly")
)

// CreateScope tells a client where the caller may create rooms.
type CreateScope string

const (
	ScopeAny     CreateScope = "any"              // with or without a parent channel
	ScopeFounded CreateScope = "founded_channels" // only under channels the caller founded
	ScopeNone    CreateScope = "none"
)

// Kind of a registry room.
const (
	KindStatic  = "static"
	KindDynamic = "dynamic"
)

const (
	MaxNameLen = 32 // runes
	// allowed_roles limits.
	MaxAllowedRoles = 20
	MaxRoleNameLen  = 64 // runes
	maxIDLen        = 32
	maxChannelLen   = 64 // Ergo limits.channellen in the stack
	maxPosition     = 1_000_000
)

type Permissions struct {
	Policy    settings.VoiceCreation
	CanCreate bool
	Scope     CreateScope
	// FoundedChannels: with ScopeFounded, the channels the caller may create
	// rooms under (casefolded, sorted).
	FoundedChannels []string
	// ManageAny: the caller can manage every dynamic room (and is exempt
	// from the per-user limit). BypassLocks: the caller passes every lock.
	ManageAny   bool
	BypassLocks bool
}

// View is one room as the caller sees it.
type View struct {
	Room      voiceroom.Record
	Kind      string
	CanManage bool
	CanJoin   bool
	CanSpeak  bool
}

type Listing struct {
	Permissions Permissions
	// Rooms in display order, filtered by the token's rooms claim.
	Rooms []View
}

type CreateInput struct {
	Name   string
	Parent string // "" = none; otherwise an IRC channel name
	// Zero values = no lock.
	MinRole                  roles.Role
	SpeakRole                roles.Role
	RequireChannelMembership bool
	// Network roles, one of which is needed to join. Empty = anyone.
	AllowedRoles []string
}

// UpdateInput: nil fields are left alone. The id and parent never change.
type UpdateInput struct {
	Name                     *string
	Position                 *int
	MinRole                  *roles.Role
	SpeakRole                *roles.Role
	RequireChannelMembership *bool
	// Replaces the list; a pointer to an empty slice clears it.
	AllowedRoles *[]string
}

type VoiceChannelServiceImpl interface {
	room.Directory
	room.JoinPolicy
	List(ctx context.Context, id *auth.Identity) (*Listing, error)
	Create(ctx context.Context, id *auth.Identity, in CreateInput) (View, error)
	Update(ctx context.Context, id *auth.Identity, roomID string, in UpdateInput) (View, error)
	// Delete removes a dynamic room from the registry. Kicking its live
	// participants is the caller's job (room.RoomServiceImpl.CloseRoom).
	Delete(ctx context.Context, id *auth.Identity, roomID string) (voiceroom.Record, error)
}

type Options struct {
	Repo     voiceroom.RoomRepositoryImpl
	Settings settings.SettingsServiceImpl
	Ergo     ergo.ErgoServiceImpl
	Metrics  *metrics.Metrics
	// Static rooms (VOICE_ROOMS), in display order.
	Static []room.Def
	// Dynamic rooms one (non-admin) user may own. Default 10.
	MaxPerUser int
	// Dynamic rooms in total. Default 500.
	MaxRooms int
	Now      func() time.Time
}

type VoiceChannelService struct {
	repo     voiceroom.RoomRepositoryImpl
	settings settings.SettingsServiceImpl
	ergo     ergo.ErgoServiceImpl
	metrics  *metrics.Metrics
	perUser  int
	maxRooms int
	now      func() time.Time

	// writeMu serialises registry changes (validation + persist + publish).
	writeMu sync.Mutex
	mu      sync.RWMutex
	rooms   map[string]voiceroom.Record
	ordered []voiceroom.Record // display order; replaced, never mutated
	defs    []room.Def         // same order; replaced, never mutated
}

// NewVoiceChannelService loads the registry and reconciles it with the
// static rooms: each VOICE_ROOMS entry is (re)written as a static room at its
// list position, and static rooms no longer listed are removed. Dynamic
// rooms are untouched (a dynamic room whose id is now in VOICE_ROOMS becomes
// static).
func NewVoiceChannelService(ctx context.Context, o Options) (*VoiceChannelService, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxPerUser <= 0 {
		o.MaxPerUser = 10
	}
	if o.MaxRooms <= 0 {
		o.MaxRooms = 500
	}
	if o.Metrics == nil {
		o.Metrics = metrics.Noop()
	}
	if o.Ergo == nil {
		o.Ergo = ergo.NewErgoService(ergo.Options{})
	}
	if o.Settings == nil {
		o.Settings = settings.NewSettingsService(settings.Options{})
	}
	s := &VoiceChannelService{
		repo: o.Repo, settings: o.Settings, ergo: o.Ergo, metrics: o.Metrics,
		perUser: o.MaxPerUser, maxRooms: o.MaxRooms, now: o.Now, rooms: map[string]voiceroom.Record{},
	}
	recs, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		s.rooms[r.ID] = r
	}
	listed := map[string]bool{}
	for i, d := range o.Static {
		listed[d.ID] = true
		old, had := s.rooms[d.ID]
		rec := voiceroom.Record{ID: d.ID, Name: d.Name, Position: i, Static: true, CreatedAt: old.CreatedAt}
		if !had {
			rec.CreatedAt = s.now().UTC().Truncate(time.Millisecond)
		}
		if had && old.Equal(rec) {
			continue
		}
		if err := s.repo.Put(ctx, rec); err != nil {
			return nil, err
		}
		s.rooms[d.ID] = rec
	}
	for id, r := range s.rooms {
		if r.Static && !listed[id] {
			if err := s.repo.Delete(ctx, id); err != nil {
				return nil, err
			}
			delete(s.rooms, id)
		}
	}
	s.publishLocked()
	s.metrics.ObserveDefinedRooms(s.stats)
	return s, nil
}

// publishLocked rebuilds the ordered views. Caller holds mu (write) or is
// the constructor.
func (s *VoiceChannelService) publishLocked() {
	ordered := make([]voiceroom.Record, 0, len(s.rooms))
	for _, r := range s.rooms {
		ordered = append(ordered, r)
	}
	slices.SortFunc(ordered, func(a, b voiceroom.Record) int {
		return cmp.Or(cmp.Compare(a.Position, b.Position), a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	defs := make([]room.Def, len(ordered))
	for i, r := range ordered {
		defs[i] = room.Def{ID: r.ID, Name: r.Name}
	}
	s.ordered, s.defs = ordered, defs
}

func (s *VoiceChannelService) stats() metrics.DefinedRooms {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var d metrics.DefinedRooms
	for _, r := range s.rooms {
		if r.Static {
			d.Static++
		} else {
			d.Dynamic++
		}
	}
	return d
}

// Defs implements room.Directory.
func (s *VoiceChannelService) Defs() []room.Def {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defs
}

// Has implements room.Directory.
func (s *VoiceChannelService) Has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.rooms[id]
	return ok
}

func (s *VoiceChannelService) get(id string) (voiceroom.Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rooms[id]
	return r, ok
}

// ---- caller: one request's view of the caller's IRC standing --------------

type caller struct {
	id   *auth.Identity
	ergo ergo.ErgoServiceImpl
	ctx  context.Context

	fetched bool
	st      *ergo.Standing
	err     error
}

func (s *VoiceChannelService) caller(ctx context.Context, id *auth.Identity) *caller {
	return &caller{id: id, ergo: s.ergo, ctx: ctx}
}

func (c *caller) standing() (*ergo.Standing, error) {
	if !c.fetched {
		c.fetched = true
		if c.id.Account == "" {
			c.st = &ergo.Standing{}
		} else {
			c.st, c.err = c.ergo.Standing(c.ctx, c.id.Account)
		}
	}
	return c.st, c.err
}

// admin: network admin for management decisions: manage any dynamic room,
// create under every policy, no per-user limit, may set oper locks. Tokens
// with a perms claim: voice.manage_any (the role claim is ignored); older
// tokens: role moderator/admin.
func (c *caller) admin() bool {
	if c.id.HasNetworkPerms() {
		return c.id.HasPerm(auth.PermVoiceManageAny)
	}
	return c.id.IsStaff()
}

// bypass: passes every join/speak lock (min_role, speak_role,
// require_channel_membership, allowed_roles). Tokens with a perms claim:
// voice.bypass_locks; older tokens: role moderator/admin.
func (c *caller) bypass() bool {
	if c.id.HasNetworkPerms() {
		return c.id.HasPerm(auth.PermVoiceBypassLocks)
	}
	return c.id.IsStaff()
}

// createPerm: the token allows creating rooms at all (the policy decides
// where). Tokens with a perms claim need voice.create or voice.manage_any;
// older tokens always pass this gate.
func (c *caller) createPerm() bool {
	return !c.id.HasNetworkPerms() || c.id.HasPerm(auth.PermVoiceCreate) || c.id.HasPerm(auth.PermVoiceManageAny)
}

// legacyAdmin: an auth admin by the role claim on a token without perms.
// Only those are oper (and so founder) in every channel; with perms the
// IRC role is always the real one.
func (c *caller) legacyAdmin() bool {
	return !c.id.HasNetworkPerms() && c.id.IsStaff()
}

// hasAllowedRole: rec has no allowed_roles, or the caller holds one of them.
// Tokens without a roles claim hold none.
func (c *caller) hasAllowedRole(rec voiceroom.Record) bool {
	return len(rec.AllowedRoles) == 0 || slices.ContainsFunc(rec.AllowedRoles, c.id.HasNetworkRole)
}

// role is the caller's role in channel ("" = no channel: Member).
func (c *caller) role(channel string) (roles.Role, error) {
	if c.legacyAdmin() {
		return roles.Oper, nil
	}
	if channel == "" {
		return roles.Member, nil
	}
	st, err := c.standing()
	if err != nil {
		return roles.Member, err
	}
	return st.Role(channel), nil
}

func (c *caller) inChannel(channel string) (bool, error) {
	st, err := c.standing()
	if err != nil {
		return false, err
	}
	return st.In(channel), nil
}

// founds: the caller is the founder of channel (registered founder or +q).
func (c *caller) founds(channel string) (bool, error) {
	if channel == "" {
		return false, nil
	}
	r, err := c.role(channel)
	return r >= roles.Founder, err
}

// ---- decisions ---------------------------------------------------------------

// access decides join/speak for rec. err is only set when an Ergo lookup
// was needed and failed (canJoin is then false).
func (c *caller) access(rec voiceroom.Record) (canJoin, canSpeak bool, err error) {
	if !c.id.CanJoin(rec.ID) {
		return false, false, nil
	}
	if c.bypass() {
		return true, c.id.CanSpeak(), nil
	}
	if !c.hasAllowedRole(rec) {
		return false, false, nil
	}
	tokenSpeak := c.id.CanSpeak()
	if rec.Parent == "" {
		// Only the registered (member) vs admins (oper) lock applies.
		canJoin = rec.MinRole < roles.Oper
		return canJoin, canJoin && tokenSpeak && rec.SpeakRole < roles.Oper, nil
	}
	if rec.MinRole == roles.Member && rec.SpeakRole == roles.Member && !rec.RequireChannelMembership {
		return true, tokenSpeak, nil
	}
	role, err := c.role(rec.Parent)
	if err != nil {
		return false, false, err
	}
	canJoin = role >= rec.MinRole
	if canJoin && rec.RequireChannelMembership {
		in, err := c.inChannel(rec.Parent)
		if err != nil {
			return false, false, err
		}
		canJoin = in
	}
	return canJoin, canJoin && tokenSpeak && role >= rec.SpeakRole, nil
}

// canManage: dynamic room, and the caller is an admin (voice.manage_any),
// its owner, or the founder of its parent channel.
func (c *caller) canManage(rec voiceroom.Record) (bool, error) {
	if rec.Static {
		return false, nil
	}
	if c.admin() || (rec.OwnerUserID != "" && rec.OwnerUserID == c.id.UserID) {
		return true, nil
	}
	return c.founds(rec.Parent)
}

func (s *VoiceChannelService) permissions(c *caller) Permissions {
	p := Permissions{Policy: s.settings.VoiceChannelCreation(c.ctx), Scope: ScopeNone,
		ManageAny: c.admin(), BypassLocks: c.bypass()}
	switch {
	case c.admin():
		p.Scope, p.CanCreate = ScopeAny, true
	case !c.createPerm():
	case p.Policy == settings.CreateRegistered:
		p.Scope, p.CanCreate = ScopeAny, true
	case p.Policy == settings.CreateFounders:
		p.Scope = ScopeFounded
		p.FoundedChannels = []string{}
		if st, err := c.standing(); err == nil {
			p.FoundedChannels = st.FoundedChannels()
			slices.Sort(p.FoundedChannels)
		}
		p.CanCreate = len(p.FoundedChannels) > 0
	}
	return p
}

// AuthorizeJoin implements room.JoinPolicy.
func (s *VoiceChannelService) AuthorizeJoin(ctx context.Context, roomID string, id *auth.Identity) (bool, error) {
	rec, ok := s.get(roomID)
	if !ok {
		return false, room.ErrNotFound
	}
	c := s.caller(ctx, id)
	canJoin, canSpeak, err := c.access(rec)
	if err != nil {
		logger.FromCtx(ctx).Warn().Err(err).Str("room", roomID).Msg("IRC role check failed; join refused")
		return false, fmt.Errorf("%w: %v", room.ErrForbidden, ErrUnavailable)
	}
	if !canJoin {
		return false, fmt.Errorf("%w: %s", room.ErrForbidden, lockText(c, rec))
	}
	return canSpeak, nil
}

func lockText(c *caller, r voiceroom.Record) string {
	switch {
	case !c.hasAllowedRole(r):
		return "needs one of these roles: " + strings.Join(r.AllowedRoles, ", ")
	case r.MinRole >= roles.Oper:
		return "network admins only"
	case r.Parent == "":
		return "not allowed"
	case r.RequireChannelMembership && r.MinRole == roles.Member:
		return "join " + r.Parent + " on IRC first"
	case r.RequireChannelMembership:
		return "needs " + r.MinRole.String() + " in " + r.Parent + " and to be in the channel on IRC"
	}
	return "needs " + r.MinRole.String() + " or higher in " + r.Parent
}

func (s *VoiceChannelService) view(c *caller, rec voiceroom.Record) View {
	v := View{Room: rec, Kind: KindDynamic}
	if rec.Static {
		v.Kind = KindStatic
	}
	var err error
	v.CanJoin, v.CanSpeak, err = c.access(rec)
	if err == nil {
		v.CanManage, err = c.canManage(rec)
	}
	if err != nil {
		logger.FromCtx(c.ctx).Debug().Err(err).Str("room", rec.ID).Msg("IRC role check failed; showing as locked")
	}
	return v
}

func (s *VoiceChannelService) List(ctx context.Context, id *auth.Identity) (*Listing, error) {
	c := s.caller(ctx, id)
	s.mu.RLock()
	ordered := s.ordered
	s.mu.RUnlock()
	l := &Listing{Permissions: s.permissions(c), Rooms: make([]View, 0, len(ordered))}
	for _, rec := range ordered {
		if id.CanJoin(rec.ID) {
			l.Rooms = append(l.Rooms, s.view(c, rec))
		}
	}
	return l, nil
}

// ---- changes -----------------------------------------------------------------

func outcome(err error) metrics.RoomChangeOutcome {
	switch {
	case err == nil:
		return metrics.RoomChangeOK
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrStatic):
		return metrics.RoomChangeForbidden
	case errors.Is(err, ErrInvalid):
		return metrics.RoomChangeInvalid
	case errors.Is(err, ErrConflict):
		return metrics.RoomChangeConflict
	case errors.Is(err, ErrNotFound):
		return metrics.RoomChangeNotFound
	case errors.Is(err, ErrLimit):
		return metrics.RoomChangeLimit
	case errors.Is(err, ErrUnavailable):
		return metrics.RoomChangeUnavailable
	}
	return metrics.RoomChangeError
}

func (s *VoiceChannelService) Create(ctx context.Context, id *auth.Identity, in CreateInput) (v View, err error) {
	defer func() { s.metrics.RoomChange(ctx, metrics.RoomCreate, outcome(err)) }()
	c := s.caller(ctx, id)

	name, err := cleanName(in.Name)
	if err != nil {
		return View{}, err
	}
	parent, err := cleanChannel(in.Parent)
	if err != nil {
		return View{}, err
	}
	rec := voiceroom.Record{Name: name, Parent: parent}
	if err := applyLocks(c, &rec, &in.MinRole, &in.SpeakRole, &in.RequireChannelMembership, &in.AllowedRoles); err != nil {
		return View{}, err
	}

	// Precedence (docs/ROOMS.md): admin (voice.manage_any, or the legacy
	// role on tokens without perms) anywhere; otherwise the token must allow
	// creating (voice.create, if it has perms) and then the policy decides.
	switch p := s.settings.VoiceChannelCreation(ctx); {
	case c.admin():
	case !c.createPerm():
		return View{}, fmt.Errorf("%w: you don't have permission to create voice channels", ErrForbidden)
	case p == settings.CreateRegistered:
	case p == settings.CreateFounders:
		if parent == "" {
			return View{}, fmt.Errorf("%w: voice channels must belong to an IRC channel you founded", ErrForbidden)
		}
		ok, err := c.founds(parent)
		if err != nil {
			return View{}, fmt.Errorf("%w (%v)", ErrUnavailable, err)
		}
		if !ok {
			return View{}, fmt.Errorf("%w: only the founder of %s can create voice channels in it", ErrForbidden, parent)
		}
	default:
		return View{}, fmt.Errorf("%w: only network admins can create voice channels", ErrForbidden)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.RLock()
	owned, dynamic, maxPos := 0, 0, -1
	for _, r := range s.rooms {
		maxPos = max(maxPos, r.Position)
		if r.Static {
			continue
		}
		dynamic++
		if r.OwnerUserID == id.UserID {
			owned++
		}
	}
	conflict := s.nameTakenLocked(parent, name, "")
	s.mu.RUnlock()
	if conflict {
		return View{}, ErrConflict
	}
	if dynamic >= s.maxRooms {
		return View{}, fmt.Errorf("%w: this server has %d voice channels", ErrLimit, dynamic)
	}
	if !c.admin() && owned >= s.perUser {
		return View{}, fmt.Errorf("%w: you already own %d voice channels", ErrLimit, owned)
	}

	rec.ID = s.newID(parent, name)
	rec.OwnerUserID, rec.OwnerName = id.UserID, id.DisplayName
	rec.CreatedAt = s.now().UTC().Truncate(time.Millisecond)
	rec.Position = min(maxPos+1, maxPosition)
	if err := s.store(ctx, rec); err != nil {
		return View{}, err
	}
	logger.FromCtx(ctx).Info().Str("room", rec.ID).Str("parent", parent).Str("owner", id.UserID).Msg("voice room created")
	return s.view(c, rec), nil
}

func (s *VoiceChannelService) Update(ctx context.Context, id *auth.Identity, roomID string, in UpdateInput) (v View, err error) {
	defer func() { s.metrics.RoomChange(ctx, metrics.RoomUpdate, outcome(err)) }()
	c := s.caller(ctx, id)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rec, err := s.manageable(c, roomID)
	if err != nil {
		return View{}, err
	}
	if in.Name != nil {
		name, err := cleanName(*in.Name)
		if err != nil {
			return View{}, err
		}
		s.mu.RLock()
		taken := s.nameTakenLocked(rec.Parent, name, rec.ID)
		s.mu.RUnlock()
		if taken {
			return View{}, ErrConflict
		}
		rec.Name = name
	}
	if in.Position != nil {
		if *in.Position < 0 || *in.Position > maxPosition {
			return View{}, fmt.Errorf("%w: position must be 0..%d", ErrInvalid, maxPosition)
		}
		rec.Position = *in.Position
	}
	if err := applyLocks(c, &rec, in.MinRole, in.SpeakRole, in.RequireChannelMembership, in.AllowedRoles); err != nil {
		return View{}, err
	}
	if err := s.store(ctx, rec); err != nil {
		return View{}, err
	}
	return s.view(c, rec), nil
}

func (s *VoiceChannelService) Delete(ctx context.Context, id *auth.Identity, roomID string) (rec voiceroom.Record, err error) {
	defer func() { s.metrics.RoomChange(ctx, metrics.RoomDelete, outcome(err)) }()
	c := s.caller(ctx, id)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rec, err = s.manageable(c, roomID)
	if err != nil {
		return voiceroom.Record{}, err
	}
	if err := s.repo.Delete(ctx, rec.ID); err != nil {
		return voiceroom.Record{}, err
	}
	s.mu.Lock()
	delete(s.rooms, rec.ID)
	s.publishLocked()
	s.mu.Unlock()
	logger.FromCtx(ctx).Info().Str("room", rec.ID).Str("by", id.UserID).Msg("voice room deleted")
	return rec, nil
}

// manageable loads roomID and checks the caller may change it. Caller holds writeMu.
func (s *VoiceChannelService) manageable(c *caller, roomID string) (voiceroom.Record, error) {
	rec, ok := s.get(roomID)
	if !ok || !c.id.CanJoin(roomID) {
		return voiceroom.Record{}, ErrNotFound
	}
	if rec.Static {
		return voiceroom.Record{}, ErrStatic
	}
	ok, err := c.canManage(rec)
	if err != nil {
		return voiceroom.Record{}, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	if !ok {
		return voiceroom.Record{}, fmt.Errorf("%w: only the room's creator, the founder of its channel or a network admin can change it", ErrForbidden)
	}
	return rec, nil
}

// store persists rec and publishes it. Caller holds writeMu.
func (s *VoiceChannelService) store(ctx context.Context, rec voiceroom.Record) error {
	if err := s.repo.Put(ctx, rec); err != nil {
		return err
	}
	s.mu.Lock()
	s.rooms[rec.ID] = rec
	s.publishLocked()
	s.mu.Unlock()
	return nil
}

// nameTakenLocked: another room under parent has name (case-insensitive). Caller holds mu.
func (s *VoiceChannelService) nameTakenLocked(parent, name, except string) bool {
	for _, r := range s.rooms {
		if r.ID != except && r.Parent == parent && strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

// applyLocks validates and applies lock changes (nil = unchanged).
func applyLocks(c *caller, rec *voiceroom.Record, minRole, speakRole *roles.Role, requireMember *bool, allowedRoles *[]string) error {
	if allowedRoles != nil {
		ar, err := cleanAllowedRoles(*allowedRoles)
		if err != nil {
			return err
		}
		rec.AllowedRoles = ar
	}
	if minRole != nil {
		rec.MinRole = *minRole
	}
	if speakRole != nil {
		rec.SpeakRole = *speakRole
	}
	if requireMember != nil {
		rec.RequireChannelMembership = *requireMember
	}
	if rec.Parent == "" {
		for _, r := range []roles.Role{rec.MinRole, rec.SpeakRole} {
			if r != roles.Member && r != roles.Oper {
				return fmt.Errorf("%w: rooms without a channel can only be locked to member or oper", ErrInvalid)
			}
		}
		if rec.RequireChannelMembership {
			return fmt.Errorf("%w: require_channel_membership needs a parent channel", ErrInvalid)
		}
	}
	if !c.admin() && ((minRole != nil && *minRole == roles.Oper) || (speakRole != nil && *speakRole == roles.Oper)) {
		return fmt.Errorf("%w: only network admins can lock a room to oper", ErrForbidden)
	}
	return nil
}

// ---- validation and ids ------------------------------------------------------

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n == 0 || n > MaxNameLen || !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: name must be 1-%d characters", ErrInvalid, MaxNameLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "", fmt.Errorf("%w: name has control characters", ErrInvalid)
		}
	}
	return s, nil
}

// cleanAllowedRoles trims, validates and dedupes (case-insensitively, first
// spelling wins) a list of network role names. Empty = nil (no restriction).
func cleanAllowedRoles(in []string) ([]string, error) {
	var out []string
	for _, r := range in {
		r = strings.TrimSpace(r)
		n := utf8.RuneCountInString(r)
		if n == 0 || n > MaxRoleNameLen || !utf8.ValidString(r) {
			return nil, fmt.Errorf("%w: allowed_roles entries must be 1-%d characters", ErrInvalid, MaxRoleNameLen)
		}
		if strings.ContainsFunc(r, unicode.IsControl) {
			return nil, fmt.Errorf("%w: allowed_roles entry has control characters", ErrInvalid)
		}
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, r) }) {
			out = append(out, r)
		}
	}
	if len(out) > MaxAllowedRoles {
		return nil, fmt.Errorf("%w: at most %d allowed_roles", ErrInvalid, MaxAllowedRoles)
	}
	return out, nil
}

// cleanChannel validates an IRC channel name and casefolds it ("" stays "").
func cleanChannel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) < 2 || len(s) > maxChannelLen || s[0] != '#' || !utf8.ValidString(s) ||
		strings.ContainsAny(s, " ,\a:*?!@") || strings.ContainsFunc(s, unicode.IsControl) {
		return "", fmt.Errorf("%w: parent must be an IRC channel name like #boson", ErrInvalid)
	}
	return ergo.Casefold(s), nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// newID makes a unique [a-z0-9-]{1,32} id from the parent and name, e.g.
// "#boson" + "Game Night" → "boson-game-night" (then -2, -3, … if taken).
// Caller holds writeMu.
func (s *VoiceChannelService) newID(parent, name string) string {
	base := slugify(name)
	if base == "" {
		base = "room"
	}
	if p := slugify(strings.TrimPrefix(parent, "#")); p != "" {
		base = p + "-" + base
	}
	base = strings.Trim(truncate(base, maxIDLen), "-")
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := base
	for n := 2; ; n++ {
		if _, taken := s.rooms[id]; !taken {
			return id
		}
		suffix := "-" + strconv.Itoa(n)
		id = strings.Trim(truncate(base, maxIDLen-len(suffix)), "-") + suffix
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
