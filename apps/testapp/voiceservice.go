package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/boson-chat/quicvoice/client"
	"github.com/boson-chat/quicvoice/client/voice"
	"github.com/boson-chat/quicvoice/gen/controlpb"
	"github.com/golang-jwt/jwt/v5"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// Events sent to the frontend.
const (
	EventState = "voice:state" // State, ~10 Hz
	EventLog   = "voice:log"   // LogEntry
)

func init() {
	application.RegisterEvent[State](EventState)
	application.RegisterEvent[LogEntry](EventLog)
}

type ConnectRequest struct {
	Addr     string `json:"addr"`
	Token    string `json:"token"`
	Room     string `json:"room"`
	Insecure bool   `json:"insecure"`
}

type Devices struct {
	Inputs  []voice.DeviceInfo `json:"inputs"`
	Outputs []voice.DeviceInfo `json:"outputs"`
}

type Participant struct {
	SessionID   int     `json:"sessionId"`
	Name        string  `json:"name"`
	Role        string  `json:"role"`
	Self        bool    `json:"self"`
	Muted       bool    `json:"muted"`
	Deafened    bool    `json:"deafened"`
	Speaking    bool    `json:"speaking"`
	Level       float32 `json:"level"`
	Volume      int     `json:"volume"` // percent
	LossPercent float32 `json:"lossPercent"`
	BufferMs    int     `json:"bufferMs"`
}

type State struct {
	Connected    bool          `json:"connected"`
	Connecting   bool          `json:"connecting"`
	Addr         string        `json:"addr"`
	Room         string        `json:"room"`
	Participants []Participant `json:"participants"`
	MicLevel     float32       `json:"micLevel"`
	Transmitting bool          `json:"transmitting"`
	Muted        bool          `json:"muted"`
	Deafened     bool          `json:"deafened"`
	PushToTalk   bool          `json:"pushToTalk"`
	Loopback     bool          `json:"loopback"`
	AudioError   string        `json:"audioError"`
	RTTMs        float64       `json:"rttMs"`
	FramesSent   uint64        `json:"framesSent"`
	FramesRecv   uint64        `json:"framesRecv"`
}

type LogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"` // info | warn | error
	Message string `json:"message"`
}

type member struct {
	name, role       string
	muted, deafened  bool
}

// VoiceService is bound to the frontend. All audio stays in Go; the UI only
// sends commands and renders State.
type VoiceService struct {
	audio  *voice.Audio
	engine *voice.Engine

	mu         sync.Mutex
	conn       *client.Client
	connecting bool
	addr, room string
	self       uint16
	members    map[uint16]*member
	volumes    map[uint16]int
	inputID    string
	outputID   string
	muted      bool
	deafened   bool
	ptt        bool
	loopback   bool
	audioErr   string

	stop chan struct{}
}

func NewVoiceService() *VoiceService {
	return &VoiceService{members: map[uint16]*member{}, volumes: map[uint16]int{}, stop: make(chan struct{})}
}

func (s *VoiceService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	eng, err := voice.NewEngine(voice.Options{})
	if err != nil {
		return err
	}
	s.engine = eng
	if s.audio, err = voice.NewAudio(); err != nil {
		return err
	}
	// Opening the mic can block on the OS permission prompt; don't hold up
	// window creation for it.
	go s.restartAudio()
	go s.stateLoop()
	return nil
}

func (s *VoiceService) ServiceShutdown() error {
	close(s.stop)
	_ = s.Disconnect()
	if s.audio != nil {
		s.audio.Close()
	}
	if s.engine != nil {
		s.engine.Close()
	}
	return nil
}

// --- devices ---

func (s *VoiceService) Devices() (Devices, error) {
	in, err := s.audio.Inputs()
	if err != nil {
		return Devices{}, err
	}
	out, err := s.audio.Outputs()
	if err != nil {
		return Devices{}, err
	}
	return Devices{Inputs: in, Outputs: out}, nil
}

// SetDevices switches mic/speakers. Empty strings mean system default.
func (s *VoiceService) SetDevices(inputID, outputID string) {
	s.mu.Lock()
	s.inputID, s.outputID = inputID, outputID
	s.mu.Unlock()
	s.restartAudio()
}

func (s *VoiceService) restartAudio() {
	s.mu.Lock()
	in, out := s.inputID, s.outputID
	s.mu.Unlock()
	err := s.audio.Start(s.engine, in, out)
	s.mu.Lock()
	s.audioErr = ""
	if err != nil {
		s.audioErr = err.Error()
	}
	s.mu.Unlock()
	if err != nil {
		s.log("error", "audio: "+err.Error())
	}
}

// --- connection ---

func (s *VoiceService) Connect(req ConnectRequest) error {
	if req.Addr == "" || req.Room == "" || req.Token == "" {
		return errors.New("server, room and token are required")
	}
	s.mu.Lock()
	if s.conn != nil || s.connecting {
		s.mu.Unlock()
		return errors.New("already connected")
	}
	s.connecting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.connecting = false
		s.mu.Unlock()
	}()

	s.log("info", fmt.Sprintf("connecting to %s…", req.Addr))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, client.Config{
		Addr:               req.Addr,
		Token:              req.Token,
		InsecureSkipVerify: req.Insecure,
		OnAudio:            s.engine.HandleAudio,
		OnControl:          s.onControl,
	})
	if err != nil {
		s.log("error", err.Error())
		return err
	}
	state, err := c.Join(ctx, req.Room)
	if err != nil {
		_ = c.Close()
		s.log("error", err.Error())
		return err
	}

	s.mu.Lock()
	s.conn, s.addr, s.room = c, req.Addr, state.GetRoom()
	s.self = uint16(state.GetYourSessionId())
	clear(s.members)
	for _, p := range state.GetParticipants() {
		s.members[uint16(p.GetSessionId())] = memberFrom(p)
	}
	muted, deafened := s.muted, s.deafened
	s.mu.Unlock()

	s.engine.SetSender(c)
	if muted || deafened {
		_ = c.SetMute(muted, deafened)
	}
	s.log("info", fmt.Sprintf("joined %q as session %d (%d here)", state.GetRoom(), state.GetYourSessionId(), len(state.GetParticipants())))

	go s.watch(c)
	return nil
}

func (s *VoiceService) Disconnect() error {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Close() // watch() does the cleanup
}

// watch pings for RTT and cleans up when the connection ends.
func (s *VoiceService) watch(c *client.Client) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	_ = c.Ping()
	for {
		select {
		case <-t.C:
			_ = c.Ping()
		case <-c.Done():
			s.engine.SetSender(nil)
			s.mu.Lock()
			if s.conn == c {
				s.conn = nil
				for sid := range s.members {
					s.engine.RemoveSender(sid)
				}
				clear(s.members)
			}
			s.mu.Unlock()
			s.log("warn", "disconnected")
			return
		}
	}
}

func (s *VoiceService) onControl(m *controlpb.ControlMessage) {
	switch {
	case m.GetParticipantJoined() != nil:
		p := m.GetParticipantJoined().GetParticipant()
		s.mu.Lock()
		s.members[uint16(p.GetSessionId())] = memberFrom(p)
		s.mu.Unlock()
		s.log("info", p.GetDisplayName()+" joined")
	case m.GetParticipantLeft() != nil:
		sid := uint16(m.GetParticipantLeft().GetSessionId())
		s.mu.Lock()
		name := "someone"
		if mem, ok := s.members[sid]; ok {
			name = mem.name
		}
		delete(s.members, sid)
		s.mu.Unlock()
		s.engine.RemoveSender(sid)
		s.log("info", name+" left")
	case m.GetParticipantUpdated() != nil:
		p := m.GetParticipantUpdated().GetParticipant()
		s.mu.Lock()
		s.members[uint16(p.GetSessionId())] = memberFrom(p)
		s.mu.Unlock()
	case m.GetError() != nil:
		e := m.GetError()
		s.log("error", fmt.Sprintf("server: %s: %s", e.GetCode(), e.GetMessage()))
	}
}

func memberFrom(p *controlpb.Participant) *member {
	return &member{
		name:     p.GetDisplayName(),
		role:     p.GetRole().String(),
		muted:    p.GetSelfMuted() || p.GetServerMuted(),
		deafened: p.GetDeafened(),
	}
}

// --- controls ---

func (s *VoiceService) SetMuted(muted bool) { s.setMute(&muted, nil) }

func (s *VoiceService) SetDeafened(deafened bool) { s.setMute(nil, &deafened) }

func (s *VoiceService) setMute(muted, deafened *bool) {
	s.mu.Lock()
	if muted != nil {
		s.muted = *muted
	}
	if deafened != nil {
		s.deafened = *deafened
	}
	m, d, c := s.muted, s.deafened, s.conn
	s.mu.Unlock()

	// Deafened implies muted, like most voice apps.
	s.engine.SetMuted(m || d)
	s.engine.SetDeafened(d)
	if c != nil {
		_ = c.SetMute(m || d, d)
	}
}

func (s *VoiceService) SetPushToTalk(enabled bool) {
	s.mu.Lock()
	s.ptt = enabled
	s.mu.Unlock()
	if enabled {
		s.engine.SetMode(voice.ModePushToTalk)
	} else {
		s.engine.SetMode(voice.ModeVoiceActivity)
	}
	s.engine.SetPTT(false)
}

// PTT is called on key/button down and up.
func (s *VoiceService) PTT(down bool) { s.engine.SetPTT(down) }

// SetLoopback plays your own encoded voice back to you (mic test).
func (s *VoiceService) SetLoopback(on bool) {
	s.mu.Lock()
	s.loopback = on
	s.mu.Unlock()
	s.engine.SetLoopback(on)
	if !on {
		s.engine.RemoveSender(voice.LoopbackSession)
	}
}

// SetUserVolume sets one participant's volume, 0..200 %.
func (s *VoiceService) SetUserVolume(sessionID int, percent int) {
	percent = max(0, min(200, percent))
	s.mu.Lock()
	s.volumes[uint16(sessionID)] = percent
	s.mu.Unlock()
	s.engine.SetUserVolume(uint16(sessionID), float32(percent)/100)
}

func (s *VoiceService) SetMasterVolume(percent int) {
	s.engine.SetMasterVolume(float32(max(0, min(200, percent))) / 100)
}

// --- test tokens ---

// CanMintTokens reports whether JWT_SECRET was set when the app started, so
// the UI can offer to generate a token instead of pasting one.
func (s *VoiceService) CanMintTokens() bool { return os.Getenv("JWT_SECRET") != "" }

// MintToken signs a 12 h speaker token for user with $JWT_SECRET.
func (s *VoiceService) MintToken(user string) (string, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return "", errors.New("JWT_SECRET is not set")
	}
	if user == "" {
		return "", errors.New("name is required")
	}
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  user,
		"name": user,
		"role": "speaker",
		"iat":  now.Unix(),
		"exp":  now.Add(12 * time.Hour).Unix(),
	}).SignedString([]byte(secret))
}

// --- state to the UI ---

func (s *VoiceService) stateLoop() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if app := application.Get(); app != nil {
				app.Event.Emit(EventState, s.snapshot())
			}
		}
	}
}

func (s *VoiceService) snapshot() State {
	es := s.engine.Stats()
	senders := map[uint16]voice.SenderState{}
	for _, st := range es.Senders {
		senders[st.SessionID] = st
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{
		Connected:    s.conn != nil,
		Connecting:   s.connecting,
		Addr:         s.addr,
		Room:         s.room,
		MicLevel:     es.MicLevel,
		Transmitting: es.Transmitting,
		Muted:        s.muted,
		Deafened:     s.deafened,
		PushToTalk:   s.ptt,
		Loopback:     s.loopback,
		AudioError:   s.audioErr,
		FramesSent:   es.FramesSent,
	}
	if s.conn != nil {
		cs := s.conn.Stats()
		st.RTTMs = float64(cs.RTT.Microseconds()) / 1000
		st.FramesRecv = cs.Received
	}
	for sid, m := range s.members {
		p := Participant{
			SessionID: int(sid), Name: m.name, Role: m.role, Self: sid == s.self,
			Muted: m.muted, Deafened: m.deafened, Volume: 100,
		}
		if v, ok := s.volumes[sid]; ok {
			p.Volume = v
		}
		if p.Self {
			p.Speaking, p.Level = es.Transmitting, es.MicLevel
		} else if snd, ok := senders[sid]; ok {
			p.Speaking, p.Level = snd.Speaking, snd.Level
			j := snd.Jitter
			if total := j.Played + j.Concealed; total > 0 {
				p.LossPercent = 100 * float32(j.Concealed) / float32(total)
			}
			p.BufferMs = j.Depth * 20
		}
		st.Participants = append(st.Participants, p)
	}
	sort.Slice(st.Participants, func(i, j int) bool {
		a, b := st.Participants[i], st.Participants[j]
		if a.Self != b.Self {
			return a.Self
		}
		return a.SessionID < b.SessionID
	})
	return st
}

func (s *VoiceService) log(level, msg string) {
	if app := application.Get(); app != nil {
		app.Event.Emit(EventLog, LogEntry{Time: time.Now().Format("15:04:05"), Level: level, Message: msg})
	}
}
