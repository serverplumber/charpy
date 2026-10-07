package wire

import (
	"fmt"
	"io"
)

// MaxFrame is the most charpy reads of one frame before it stops reading.
//
// It is charpy's number, not the protocol's: MCP sets no frame size, so a
// frame past it is reported as what it is -- a frame past charpy's cap --
// and never as a violation. It is generous because a correct subject can
// answer resources/read with a large blob, and a cap that cut one would be
// charpy misreporting a correct server. What it exists to stop is a body that
// never ends taking charpy down with it.
const MaxFrame = 4 << 20

// FrameTooLarge is a frame charpy stopped reading at MaxFrame. The peer that
// sent it is still sending, or was; Read is how much charpy took before it
// stopped, a lower bound on the frame's length and never its true length.
type FrameTooLarge struct {
	Read   int
	Prefix []byte // the first bytes of the frame, as received
}

func (e *FrameTooLarge) Error() string {
	return fmt.Sprintf("wire: frame exceeds %d bytes (stopped after %d)", MaxFrame, e.Read)
}

// ReadFrame reads a whole frame -- an HTTP body -- from r, or a
// *FrameTooLarge once it has read more than MaxFrame. It never returns a frame
// cut short at the cap: a body silently truncated there would look exactly
// like one a subject cut off.
func ReadFrame(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFrame+1))
	if len(b) > MaxFrame {
		return nil, &FrameTooLarge{Read: len(b), Prefix: b}
	}
	return b, err
}
