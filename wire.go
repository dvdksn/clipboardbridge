//go:build unix

package clipboardbridge

// Minimal Wayland wire-protocol codec — just enough to act as a server for the
// wlr-data-control clipboard protocol. The Wayland wire format frames every
// message as a 2-word header (object id, then size<<16|opcode) followed by the
// argument bytes, all in host byte order. File descriptors are not part of the
// byte stream; they travel out-of-band as SCM_RIGHTS ancillary data and are
// matched to fd-typed arguments in arrival order.
//
// We only ever *receive* one fd-bearing request (offer.receive) and never send
// fds as a server, which keeps the codec small.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// order is the wire byte order. Wayland uses the host's native order; every
// platform we run the sandbox on (linux/amd64, linux/arm64) is little-endian.
var order = binary.LittleEndian

// message is a single decoded request from the client. args holds the raw
// argument bytes (everything after the 8-byte header); decode them with
// argReader. The size-framed header lets us skip requests we don't implement
// without desyncing the stream.
type message struct {
	objectID uint32
	opcode   uint16
	args     []byte
}

// conn buffers the unix socket so we can reassemble size-framed messages across
// arbitrary read boundaries, and queues file descriptors delivered out-of-band.
type conn struct {
	uc  *net.UnixConn
	buf []byte // unparsed bytes carried over between reads
	fds []int  // received fds not yet consumed by an fd-typed argument
}

func newConn(uc *net.UnixConn) *conn { return &conn{uc: uc} }

// readMessage returns the next complete client message, reading from the socket
// as needed and accumulating any SCM_RIGHTS fds into the fd queue.
func (c *conn) readMessage() (*message, error) {
	for {
		m, ok, err := c.parseBuffered()
		if err != nil {
			return nil, err
		}
		if ok {
			return m, nil
		}
		data := make([]byte, 4096)
		oob := make([]byte, 256)
		n, oobn, _, _, err := c.uc.ReadMsgUnix(data, oob)
		if err != nil {
			return nil, err
		}
		c.buf = append(c.buf, data[:n]...)
		if oobn > 0 {
			scms, err := unix.ParseSocketControlMessage(oob[:oobn])
			if err != nil {
				return nil, fmt.Errorf("parse ancillary data: %w", err)
			}
			for i := range scms {
				fds, err := unix.ParseUnixRights(&scms[i])
				if err != nil {
					continue
				}
				c.fds = append(c.fds, fds...)
			}
		}
	}
}

// parseBuffered tries to peel one complete message off the front of the buffer.
// It returns (msg, true, nil) for a full message, (nil, false, nil) when more
// bytes are needed, and (nil, false, err) for a protocol violation. A size < 8
// header is a violation: it can never advance the buffer, so treating it as
// "need more data" would spin readMessage forever; returning an error makes the
// caller close the connection instead.
func (c *conn) parseBuffered() (*message, bool, error) {
	if len(c.buf) < 8 {
		return nil, false, nil
	}
	objectID := order.Uint32(c.buf[0:4])
	word2 := order.Uint32(c.buf[4:8])
	size := int(word2 >> 16)
	opcode := uint16(word2 & 0xffff)
	if size < 8 {
		return nil, false, fmt.Errorf("invalid wayland message size %d (< 8-byte header)", size)
	}
	if len(c.buf) < size {
		return nil, false, nil
	}
	args := make([]byte, size-8)
	copy(args, c.buf[8:size])
	c.buf = c.buf[size:]
	return &message{objectID: objectID, opcode: opcode, args: args}, true, nil
}

// popFD removes and returns the oldest unconsumed received fd. The client sends
// exactly one fd per fd-typed argument, in message order.
func (c *conn) popFD() (int, bool) {
	if len(c.fds) == 0 {
		return 0, false
	}
	fd := c.fds[0]
	c.fds = c.fds[1:]
	return fd, true
}

// writeEvent frames and sends a server→client event. body is the pre-encoded
// argument bytes (see eventBody). The server never attaches fds to events.
func (c *conn) writeEvent(objectID uint32, opcode uint16, body []byte) error {
	size := 8 + len(body)
	msg := make([]byte, size)
	order.PutUint32(msg[0:4], objectID)
	order.PutUint32(msg[4:8], uint32(size<<16)|uint32(opcode))
	copy(msg[8:], body)
	if _, err := c.uc.Write(msg); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// argReader decodes argument bytes for the few requests we actually handle.
type argReader struct {
	b   []byte
	off int
}

func newArgReader(args []byte) *argReader { return &argReader{b: args} }

func (r *argReader) uint32() (uint32, bool) {
	if r.off+4 > len(r.b) {
		return 0, false
	}
	v := order.Uint32(r.b[r.off : r.off+4])
	r.off += 4
	return v, true
}

// string decodes a Wayland string: a u32 length (including the trailing NUL)
// followed by the bytes, padded to a 4-byte boundary.
func (r *argReader) string() (string, bool) {
	n, ok := r.uint32()
	if !ok {
		return "", false
	}
	length := int(n)
	if length == 0 {
		return "", true
	}
	padded := (length + 3) &^ 3
	if r.off+padded > len(r.b) {
		return "", false
	}
	// Drop the trailing NUL.
	s := string(r.b[r.off : r.off+length-1])
	r.off += padded
	return s, true
}

// eventBody builds argument bytes for server→client events.
type eventBody struct{ buf bytes.Buffer }

func (e *eventBody) uint32(v uint32) {
	var b [4]byte
	order.PutUint32(b[:], v)
	e.buf.Write(b[:])
}

// string encodes a Wayland string with length prefix, NUL terminator, and
// padding to a 4-byte boundary.
func (e *eventBody) string(s string) {
	length := len(s) + 1 // include NUL
	e.uint32(uint32(length))
	e.buf.WriteString(s)
	e.buf.WriteByte(0)
	for pad := (4 - length%4) % 4; pad > 0; pad-- {
		e.buf.WriteByte(0)
	}
}

func (e *eventBody) bytes() []byte { return e.buf.Bytes() }
