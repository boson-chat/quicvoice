package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/boson-chat/quicvoice/gen/controlpb"
	"google.golang.org/protobuf/proto"
)

// MaxControlMessage caps a single control message. Control traffic is small;
// anything bigger is a protocol error.
const MaxControlMessage = 64 << 10

var ErrControlTooLarge = errors.New("wire: control message too large")

// ControlReader reads [varint length][ControlMessage] frames.
type ControlReader struct {
	r *bufio.Reader
}

func NewControlReader(r io.Reader) *ControlReader {
	return &ControlReader{r: bufio.NewReader(r)}
}

func (c *ControlReader) Read() (*controlpb.ControlMessage, error) {
	n, err := binary.ReadUvarint(c.r)
	if err != nil {
		return nil, err
	}
	if n > MaxControlMessage {
		return nil, ErrControlTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	m := &controlpb.ControlMessage{}
	if err := proto.Unmarshal(buf, m); err != nil {
		return nil, fmt.Errorf("wire: decode control message: %w", err)
	}
	return m, nil
}

// AppendControl appends one framed control message to dst.
func AppendControl(dst []byte, m *controlpb.ControlMessage) ([]byte, error) {
	body, err := proto.Marshal(m)
	if err != nil {
		return dst, err
	}
	if len(body) > MaxControlMessage {
		return dst, ErrControlTooLarge
	}
	dst = binary.AppendUvarint(dst, uint64(len(body)))
	return append(dst, body...), nil
}

// WriteControl writes one framed control message to w in a single Write.
func WriteControl(w io.Writer, m *controlpb.ControlMessage) error {
	b, err := AppendControl(nil, m)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
