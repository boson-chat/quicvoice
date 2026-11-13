// Package wire encodes and decodes quicvoice audio datagrams.
// See docs/PROTOCOL.md for the layout.
package wire

import (
	"encoding/binary"
	"errors"
)

const Version = 1

type Kind uint8

const (
	KindUplink   Kind = 0x0
	KindDownlink Kind = 0x1
	KindPing     Kind = 0x2
)

// Flags for audio datagrams.
const (
	FlagStart    uint8 = 1 << 0 // first frame of a talk spurt
	FlagEnd      uint8 = 1 << 1 // last frame of a talk spurt
	FlagLostPrev uint8 = 1 << 2 // encoder dropped frames before this one
)

// Target kinds on downlink.
const (
	TargetRoom   uint8 = 0
	TargetDirect uint8 = 1
	TargetGroup  uint8 = 2
)

const (
	UplinkHeaderLen   = 10
	DownlinkHeaderLen = 12
	PingLen           = 9

	LevelSilence = 127
)

var (
	ErrShort   = errors.New("wire: datagram too short")
	ErrVersion = errors.New("wire: unsupported version")
	ErrKind    = errors.New("wire: unexpected kind")
)

// Peek returns the version and kind of a datagram without decoding it.
func Peek(b []byte) (version uint8, kind Kind, err error) {
	if len(b) < 1 {
		return 0, 0, ErrShort
	}
	return b[0] >> 4, Kind(b[0] & 0x0f), nil
}

// Audio holds the fields shared by uplink and downlink audio datagrams.
type Audio struct {
	Flags     uint8
	Voice     bool  // sender VAD
	Level     uint8 // -dBov, 0 loudest .. 127 silence
	Seq       uint16
	Timestamp uint32 // 48 kHz sample clock
	Payload   []byte // opus frame; aliases the input buffer on decode
}

type Uplink struct {
	Audio
	Target uint8 // 0 = room, 1-31 = voice target
}

type Downlink struct {
	Audio
	TargetKind uint8
	SessionID  uint16
}

func levelByte(voice bool, level uint8) byte {
	if level > LevelSilence {
		level = LevelSilence
	}
	if voice {
		return 0x80 | level
	}
	return level
}

func checkHeader(b []byte, kind Kind, n int) error {
	if len(b) < n {
		return ErrShort
	}
	v, k, _ := Peek(b)
	if v != Version {
		return ErrVersion
	}
	if k != kind {
		return ErrKind
	}
	return nil
}

// AppendUplink appends the encoded datagram to dst.
func AppendUplink(dst []byte, u *Uplink) []byte {
	dst = append(dst,
		Version<<4|byte(KindUplink),
		u.Flags,
		levelByte(u.Voice, u.Level),
		u.Target,
	)
	dst = binary.BigEndian.AppendUint16(dst, u.Seq)
	dst = binary.BigEndian.AppendUint32(dst, u.Timestamp)
	return append(dst, u.Payload...)
}

func ParseUplink(b []byte) (Uplink, error) {
	if err := checkHeader(b, KindUplink, UplinkHeaderLen); err != nil {
		return Uplink{}, err
	}
	return Uplink{
		Audio: Audio{
			Flags:     b[1],
			Voice:     b[2]&0x80 != 0,
			Level:     b[2] & 0x7f,
			Seq:       binary.BigEndian.Uint16(b[4:6]),
			Timestamp: binary.BigEndian.Uint32(b[6:10]),
			Payload:   b[UplinkHeaderLen:],
		},
		Target: b[3],
	}, nil
}

// AppendDownlink appends the encoded datagram to dst.
func AppendDownlink(dst []byte, d *Downlink) []byte {
	dst = append(dst,
		Version<<4|byte(KindDownlink),
		d.Flags,
		levelByte(d.Voice, d.Level),
		d.TargetKind,
	)
	dst = binary.BigEndian.AppendUint16(dst, d.SessionID)
	dst = binary.BigEndian.AppendUint16(dst, d.Seq)
	dst = binary.BigEndian.AppendUint32(dst, d.Timestamp)
	return append(dst, d.Payload...)
}

func ParseDownlink(b []byte) (Downlink, error) {
	if err := checkHeader(b, KindDownlink, DownlinkHeaderLen); err != nil {
		return Downlink{}, err
	}
	return Downlink{
		Audio: Audio{
			Flags:     b[1],
			Voice:     b[2]&0x80 != 0,
			Level:     b[2] & 0x7f,
			Seq:       binary.BigEndian.Uint16(b[6:8]),
			Timestamp: binary.BigEndian.Uint32(b[8:12]),
			Payload:   b[DownlinkHeaderLen:],
		},
		TargetKind: b[3],
		SessionID:  binary.BigEndian.Uint16(b[4:6]),
	}, nil
}

// ForwardUplink is the server fast path: it turns a raw uplink datagram into
// a downlink for one receiver without touching seq/timestamp/payload.
func ForwardUplink(dst, uplink []byte, sessionID uint16, targetKind uint8) ([]byte, error) {
	if err := checkHeader(uplink, KindUplink, UplinkHeaderLen); err != nil {
		return dst, err
	}
	dst = append(dst, Version<<4|byte(KindDownlink), uplink[1], uplink[2], targetKind)
	dst = binary.BigEndian.AppendUint16(dst, sessionID)
	return append(dst, uplink[4:]...), nil
}

// Ping is an RTT probe, echoed back with Reply set.
type Ping struct {
	Reply  bool
	ID     uint16
	SentUS uint32 // sender's clock, microseconds, wrapping
}

func AppendPing(dst []byte, p *Ping) []byte {
	var flags byte
	if p.Reply {
		flags = 1
	}
	dst = append(dst, Version<<4|byte(KindPing), flags)
	dst = binary.BigEndian.AppendUint16(dst, p.ID)
	dst = binary.BigEndian.AppendUint32(dst, p.SentUS)
	return append(dst, 0)
}

func ParsePing(b []byte) (Ping, error) {
	if err := checkHeader(b, KindPing, PingLen); err != nil {
		return Ping{}, err
	}
	return Ping{
		Reply:  b[1]&1 != 0,
		ID:     binary.BigEndian.Uint16(b[2:4]),
		SentUS: binary.BigEndian.Uint32(b[4:8]),
	}, nil
}
