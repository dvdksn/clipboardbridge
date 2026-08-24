//go:build unix

package clipboardbridge

// bridge implements the server side of the wlr-data-control Wayland protocol,
// just enough for a clipboard reader (arboard / wl-clipboard-rs, as used by
// Codex) to paste a host image with Ctrl+V. We are not a compositor: there are
// no surfaces, input, or rendering — only a wl_seat and the data-control
// objects a paste needs.
//
// Paste flow we drive, per client connection (wl-clipboard-rs opens a fresh
// connection for each read, so querying the host once per get_data_device
// reflects the current host clipboard):
//
//	client: get_registry -> bind(wl_seat), bind(zwlr_data_control_manager_v1)
//	client: manager.get_data_device(seat)
//	server: device.data_offer(O); offer.offer("image/png"); device.selection(O)
//	client: offer.receive("image/png", fd)
//	server: write host PNG bytes to fd, close
//
// When the host clipboard holds no image we send selection(null) so the reader
// reports an empty clipboard cleanly rather than receiving zero bytes.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Wayland core / wlr-data-control object kinds we track per connection.
type objKind int

const (
	kindUnknown objKind = iota
	kindDisplay
	kindRegistry
	kindSeat
	kindManager
	kindDevice
	kindOffer
	kindSource
)

// The well-known display object id (always 1) and the start of the server's
// own id allocation range (Wayland reserves >= 0xff000000 for server-created
// objects such as the data offers we introduce).
const (
	displayID        = 1
	serverIDBase     = 0xff000000
	ifaceSeat        = "wl_seat"
	ifaceManager     = "zwlr_data_control_manager_v1"
	registryNameSeat = 1
	registryNameMgr  = 2
	// wl-clipboard-rs (arboard's backend) binds wl_seat only when it is
	// advertised at version >= 2 — it filters globals on `version >= 2` — so a
	// v1 seat is ignored and the client reports "There are no seats". v2 also
	// introduces the wl_seat.name event, which we send.
	seatVersion    = 2
	managerVersion = 1
)

// Opcodes (from the wl_display, wl_registry, wl_callback, wl_seat and
// zwlr_data_control_* protocol definitions).
const (
	// wl_display requests
	displayReqSync        = 0
	displayReqGetRegistry = 1
	// wl_display events
	displayEvtError    = 0
	displayEvtDeleteID = 1

	// wl_registry request / events
	registryReqBind     = 0
	registryEvtGlobal   = 0
	callbackEvtDone     = 0
	seatEvtCapabilities = 0
	seatEvtName         = 1 // wl_seat v2+

	// zwlr_data_control_manager_v1 requests
	managerReqCreateDataSource = 0
	managerReqGetDataDevice    = 1

	// zwlr_data_control_device_v1 request / events
	deviceReqSetSelection = 0
	deviceReqDestroy      = 1
	deviceEvtDataOffer    = 0
	deviceEvtSelection    = 1

	// zwlr_data_control_offer_v1 request / event
	offerReqReceive = 0
	offerReqDestroy = 1
	offerEvtOffer   = 0
)

// server holds the dependencies shared across all client connections.
type server struct {
	host hostClipboard
	log  *slog.Logger
	// resolveSession identifies the sandbox attach behind a client connection.
	// A field rather than a direct call so tests can drive the protocol in one
	// process; the real lookup needs the client to be a separate one.
	resolveSession func(*net.UnixConn) (string, error)
}

func newServer(host hostClipboard, log *slog.Logger) *server {
	if log == nil {
		log = slog.Default()
	}
	return &server{host: host, log: log, resolveSession: sessionIDForPeer}
}

// connState is the per-connection object table and id allocator. It also
// carries the sandbox session resolved from this client, so concurrent attaches
// read their own host clipboard rather than a shared one.
type connState struct {
	objects   map[uint32]objKind
	offerData map[uint32][]byte // cached host bytes per advertised offer
	nextID    uint32
	sessionID string // "" when the client's session could not be resolved
}

func newConnState(sessionID string) *connState {
	return &connState{
		objects:   map[uint32]objKind{displayID: kindDisplay},
		offerData: map[uint32][]byte{},
		nextID:    serverIDBase,
		sessionID: sessionID,
	}
}

func (st *connState) allocID() uint32 {
	id := st.nextID
	st.nextID++
	return id
}

// serve runs the request loop for a single client connection until it closes.
// ctx (the server's lifetime context) is threaded to the host clipboard fetch
// so it is cancelled on daemon shutdown rather than blocking on the HTTP timeout.
func (s *server) serve(ctx context.Context, uc *net.UnixConn) {
	defer uc.Close()
	c := newConn(uc)
	// Resolve the client's session once, at accept: the peer credentials are
	// fixed for the life of the connection. A failure is not fatal — the fetch
	// falls back to our own environment, and failing that reads as an empty
	// clipboard. The id itself is a capability and is never logged.
	sessionID, err := s.resolveSession(uc)
	if err != nil {
		s.log.Debug("clipboard-bridge: no session for peer", "error", err)
	}
	st := newConnState(sessionID)
	for {
		m, err := c.readMessage()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Debug("clipboard-bridge: connection ended", "error", err)
			}
			return
		}
		if err := s.dispatch(ctx, c, st, m); err != nil {
			s.log.Debug("clipboard-bridge: dispatch error", "error", err)
			return
		}
	}
}

// dispatch routes a request to the handler for its object's interface. Requests
// on unknown objects or unhandled opcodes are ignored — the size-framed wire
// format lets us skip them safely.
func (s *server) dispatch(ctx context.Context, c *conn, st *connState, m *message) error {
	switch st.objects[m.objectID] {
	case kindDisplay:
		return s.onDisplay(c, st, m)
	case kindRegistry:
		return s.onRegistry(c, st, m)
	case kindManager:
		return s.onManager(ctx, c, st, m)
	case kindDevice:
		return s.onDevice(c, st, m)
	case kindOffer:
		return s.onOffer(c, st, m)
	default:
		return nil // seat, source, or unknown: nothing to do
	}
}

func (s *server) onDisplay(c *conn, st *connState, m *message) error {
	r := newArgReader(m.args)
	switch m.opcode {
	case displayReqSync:
		callback, ok := r.uint32()
		if !ok {
			return nil
		}
		// Reply done(0), then tell the client it may recycle the callback id.
		var done eventBody
		done.uint32(0)
		if err := c.writeEvent(callback, callbackEvtDone, done.bytes()); err != nil {
			return err
		}
		var del eventBody
		del.uint32(callback)
		return c.writeEvent(displayID, displayEvtDeleteID, del.bytes())
	case displayReqGetRegistry:
		registry, ok := r.uint32()
		if !ok {
			return nil
		}
		st.objects[registry] = kindRegistry
		return s.sendGlobals(c, registry)
	}
	return nil
}

// sendGlobals advertises the two globals a clipboard reader needs: a seat and
// the data-control manager.
func (s *server) sendGlobals(c *conn, registry uint32) error {
	globals := []struct {
		name    uint32
		iface   string
		version uint32
	}{
		{registryNameSeat, ifaceSeat, seatVersion},
		{registryNameMgr, ifaceManager, managerVersion},
	}
	for _, g := range globals {
		var b eventBody
		b.uint32(g.name)
		b.string(g.iface)
		b.uint32(g.version)
		if err := c.writeEvent(registry, registryEvtGlobal, b.bytes()); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) onRegistry(c *conn, st *connState, m *message) error {
	if m.opcode != registryReqBind {
		return nil
	}
	r := newArgReader(m.args)
	name, ok := r.uint32()
	if !ok {
		return nil
	}
	// wl_registry.bind encodes the target interface inline: name, then the
	// new_id's interface string + version + id.
	iface, ok := r.string()
	if !ok {
		return nil
	}
	if _, ok = r.uint32(); !ok { // version (advertised value; ignored)
		return nil
	}
	id, ok := r.uint32()
	if !ok {
		return nil
	}
	switch {
	case name == registryNameSeat && iface == ifaceSeat:
		st.objects[id] = kindSeat
		// v2 expects a name event; send it before capabilities. A clipboard
		// reader only needs the seat handle, so advertise no input devices.
		var nm eventBody
		nm.string("seat0")
		if err := c.writeEvent(id, seatEvtName, nm.bytes()); err != nil {
			return err
		}
		var caps eventBody
		caps.uint32(0)
		return c.writeEvent(id, seatEvtCapabilities, caps.bytes())
	case name == registryNameMgr && iface == ifaceManager:
		st.objects[id] = kindManager
	}
	return nil
}

func (s *server) onManager(ctx context.Context, c *conn, st *connState, m *message) error {
	r := newArgReader(m.args)
	switch m.opcode {
	case managerReqCreateDataSource:
		// Copy direction (sandbox→host) is out of scope; register the object so
		// later requests on it are recognized and ignored.
		if id, ok := r.uint32(); ok {
			st.objects[id] = kindSource
		}
	case managerReqGetDataDevice:
		device, ok := r.uint32()
		if !ok {
			return nil
		}
		st.objects[device] = kindDevice
		return s.advertiseSelection(ctx, c, st, device)
	}
	return nil
}

func (s *server) onDevice(_ *conn, st *connState, m *message) error {
	switch m.opcode {
	case deviceReqSetSelection:
		// Sandbox→host copy is out of scope; ignore.
	case deviceReqDestroy:
		delete(st.objects, m.objectID)
	}
	return nil
}

func (s *server) onOffer(c *conn, st *connState, m *message) error {
	switch m.opcode {
	case offerReqReceive:
		// Pop the fd before parsing the mime: receive always carries one fd via
		// SCM_RIGHTS, so it must be consumed (and closed) even on a malformed
		// mime — otherwise it leaks and the fd queue desyncs for later messages.
		fd, hasFD := c.popFD()
		r := newArgReader(m.args)
		mime, ok := r.string()
		if !ok {
			if hasFD {
				_ = unix.Close(fd)
			}
			return nil
		}
		if !hasFD {
			s.log.Debug("clipboard-bridge: receive without fd")
			return nil
		}
		s.serveReceive(mime, fd, st.offerData[m.objectID])
	case offerReqDestroy:
		delete(st.objects, m.objectID)
		delete(st.offerData, m.objectID)
	}
	return nil
}

// advertiseSelection reads the host clipboard once and tells the client what's
// available: an image/png offer when present, or selection(null) when empty.
func (s *server) advertiseSelection(ctx context.Context, c *conn, st *connState, device uint32) error {
	data, err := s.host.imagePNG(ctx, st.sessionID)
	if err != nil {
		s.log.Debug("clipboard-bridge: host clipboard read failed", "error", err)
		data = nil
	}
	if len(data) == 0 {
		// No image: send a null selection so the reader sees an empty clipboard.
		var sel eventBody
		sel.uint32(0)
		return c.writeEvent(device, deviceEvtSelection, sel.bytes())
	}

	offer := st.allocID()
	st.objects[offer] = kindOffer
	st.offerData[offer] = data

	// Introduce the offer object on the device.
	var intro eventBody
	intro.uint32(offer)
	if err := c.writeEvent(device, deviceEvtDataOffer, intro.bytes()); err != nil {
		return err
	}
	// Advertise the image/png MIME type on the offer.
	var mime eventBody
	mime.string(clipboardImageType)
	if err := c.writeEvent(offer, offerEvtOffer, mime.bytes()); err != nil {
		return err
	}
	// Point the device's current selection at the offer.
	var sel eventBody
	sel.uint32(offer)
	return c.writeEvent(device, deviceEvtSelection, sel.bytes())
}

// serveReceive writes the cached host bytes to the pipe fd the client passed
// with offer.receive, then closes it. Runs in its own goroutine because the
// write blocks until the client drains the pipe. Takes ownership of fd.
func (s *server) serveReceive(mime string, fd int, data []byte) {
	go func() {
		f := os.NewFile(uintptr(fd), "clipboard-pipe")
		if f == nil {
			_ = unix.Close(fd)
			return
		}
		defer f.Close()
		if mime != clipboardImageType || len(data) == 0 {
			return // close empty: nothing to hand over
		}
		if _, err := f.Write(data); err != nil {
			// EPIPE if the reader went away early — nothing to do.
			s.log.Debug("clipboard-bridge: write to receiver failed", "error", err)
		}
	}()
}
