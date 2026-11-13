package voice

import (
	"encoding/hex"
	"fmt"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// DeviceInfo describes an input or output device. ID is opaque; empty means
// the system default.
type DeviceInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

// Audio owns the miniaudio context and the running duplex device.
type Audio struct {
	ctx *malgo.AllocatedContext

	mu  sync.Mutex
	dev *malgo.Device
}

func NewAudio() (*Audio, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("voice: init audio: %w", err)
	}
	return &Audio{ctx: ctx}, nil
}

func (a *Audio) Inputs() ([]DeviceInfo, error)  { return a.list(malgo.Capture) }
func (a *Audio) Outputs() ([]DeviceInfo, error) { return a.list(malgo.Playback) }

func (a *Audio) list(kind malgo.DeviceType) ([]DeviceInfo, error) {
	devs, err := a.ctx.Devices(kind)
	if err != nil {
		return nil, err
	}
	out := make([]DeviceInfo, 0, len(devs))
	for _, d := range devs {
		out = append(out, DeviceInfo{ID: hex.EncodeToString(d.ID[:]), Name: d.Name(), IsDefault: d.IsDefault != 0})
	}
	return out, nil
}

// Start opens one duplex device (mic + speakers on one clock, which echo
// cancellation will need later) and connects it to e. Empty ids use the
// system defaults. Restarting with other ids switches devices.
func (a *Audio) Start(e *Engine, inputID, outputID string) error {
	a.Stop()

	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
	cfg.SampleRate = SampleRate
	cfg.PeriodSizeInFrames = FrameSamples / 2 // 10 ms callbacks
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = 1
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 1
	if p, err := devicePointer(inputID); err != nil {
		return err
	} else if p != nil {
		cfg.Capture.DeviceID = p
	}
	if p, err := devicePointer(outputID); err != nil {
		return err
	} else if p != nil {
		cfg.Playback.DeviceID = p
	}

	dev, err := malgo.InitDevice(a.ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: func(out, in []byte, frames uint32) {
			if len(in) >= 2 {
				e.Capture(asInt16(in))
			}
			if len(out) >= 2 {
				e.Playback(asInt16(out))
			}
		},
	})
	if err != nil {
		return fmt.Errorf("voice: open device: %w", err)
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return fmt.Errorf("voice: start device: %w", err)
	}
	a.mu.Lock()
	a.dev = dev
	a.mu.Unlock()
	return nil
}

func (a *Audio) Stop() {
	a.mu.Lock()
	dev := a.dev
	a.dev = nil
	a.mu.Unlock()
	if dev != nil {
		_ = dev.Stop()
		dev.Uninit()
	}
}

func (a *Audio) Close() {
	a.Stop()
	_ = a.ctx.Uninit()
	a.ctx.Free()
}

// devicePointer turns an id from DeviceInfo back into what miniaudio wants.
// The C copy leaks a few hundred bytes per device switch, which is fine.
func devicePointer(id string) (unsafe.Pointer, error) {
	if id == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(id)
	if err != nil {
		return nil, fmt.Errorf("voice: bad device id: %w", err)
	}
	var did malgo.DeviceID
	copy(did[:], b)
	return did.Pointer(), nil
}

// asInt16 views little-endian S16 bytes as samples without copying.
func asInt16(b []byte) []int16 {
	return unsafe.Slice((*int16)(unsafe.Pointer(unsafe.SliceData(b))), len(b)/2)
}
