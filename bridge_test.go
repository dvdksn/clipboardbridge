//go:build unix

package clipboardbridge

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// stubClipboard is a hostClipboard returning fixed bytes. sawSessionID, when
// set, records the session id the server resolved for the connection.
type stubClipboard struct {
	data         []byte
	err          error
	sawSessionID *string
}

func (s stubClipboard) imagePNG(_ context.Context, sessionID string) ([]byte, error) {
	if s.sawSessionID != nil {
		*s.sawSessionID = sessionID
	}
	return s.data, s.err
}

// fakeClient drives the client side of the data-control protocol over a unix
// socket, reusing the package's own wire codec.
type fakeClient struct {
	t  *testing.T
	uc *net.UnixConn
	c  *conn
}

func (f *fakeClient) req(objectID uint32, opcode uint16, body []byte, fds ...int) {
	f.t.Helper()
	size := 8 + len(body)
	msg := make([]byte, size)
	order.PutUint32(msg[0:4], objectID)
	order.PutUint32(msg[4:8], uint32(size<<16)|uint32(opcode))
	copy(msg[8:], body)
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	_, _, err := f.uc.WriteMsgUnix(msg, oob, nil)
	require.NoError(f.t, err)
}

// readUntil reads events until one matching (objectID, opcode) arrives.
func (f *fakeClient) readUntil(objectID uint32, opcode uint16) *message {
	f.t.Helper()
	require.NoError(f.t, f.uc.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		m, err := f.c.readMessage()
		require.NoError(f.t, err)
		if m.objectID == objectID && m.opcode == opcode {
			return m
		}
	}
}

// newTestServer starts a server on one end of a socketpair. resolve overrides
// how the connection's sandbox session is identified; the default reports none,
// since an in-process client has no attach behind it.
func newTestServer(t *testing.T, host hostClipboard, resolve ...func(*net.UnixConn) (string, error)) *fakeClient {
	t.Helper()
	// A connected socketpair avoids the OS-specific limit on unix socket path
	// length (the macOS temp dir alone can exceed it).
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	require.NoError(t, err)
	srvConn := fileConnUnix(t, fds[0], "srv")
	cliConn := fileConnUnix(t, fds[1], "cli")

	srv := newServer(host, nil)
	srv.resolveSession = func(*net.UnixConn) (string, error) { return "", nil }
	if len(resolve) > 0 {
		srv.resolveSession = resolve[0]
	}
	go srv.serve(context.Background(), srvConn)
	t.Cleanup(func() { _ = cliConn.Close() })
	return &fakeClient{t: t, uc: cliConn, c: newConn(cliConn)}
}

// fileConnUnix wraps a raw socket fd in a *net.UnixConn. net.FileConn dups the
// fd, so the original is closed afterward.
func fileConnUnix(t *testing.T, fd int, name string) *net.UnixConn {
	t.Helper()
	f := os.NewFile(uintptr(fd), name)
	require.NotNil(t, f)
	c, err := net.FileConn(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	uc, ok := c.(*net.UnixConn)
	require.True(t, ok)
	return uc
}

// Client-allocated object ids (the wire reserves >= 0xff000000 for the server).
const (
	clRegistry = 2
	clSeat     = 3
	clManager  = 4
	clDevice   = 5
)

// handshake performs get_registry + binds + get_data_device and returns the
// device id used.
func (f *fakeClient) handshake() {
	// Ask the display for the registry.
	var b eventBody
	b.uint32(clRegistry)
	f.req(displayID, displayReqGetRegistry, b.bytes())

	// bind wl_seat
	var seat eventBody
	seat.uint32(registryNameSeat)
	seat.string(ifaceSeat)
	seat.uint32(seatVersion)
	seat.uint32(clSeat)
	f.req(clRegistry, registryReqBind, seat.bytes())

	// bind manager
	var mgr eventBody
	mgr.uint32(registryNameMgr)
	mgr.string(ifaceManager)
	mgr.uint32(managerVersion)
	mgr.uint32(clManager)
	f.req(clRegistry, registryReqBind, mgr.bytes())

	// Create a data device on the manager for the seat.
	var dev eventBody
	dev.uint32(clDevice)
	dev.uint32(clSeat)
	f.req(clManager, managerReqGetDataDevice, dev.bytes())
}

func TestPasteFlow_ServesImageBytes(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nHELLO-WORLD-IMAGE")
	f := newTestServer(t, stubClipboard{data: png})
	f.handshake()

	// Server should introduce an offer, advertise image/png, and set selection.
	dataOffer := f.readUntil(clDevice, deviceEvtDataOffer)
	r := newArgReader(dataOffer.args)
	offerID, ok := r.uint32()
	require.True(t, ok)

	mime := f.readUntil(offerID, offerEvtOffer)
	mr := newArgReader(mime.args)
	gotMime, ok := mr.string()
	require.True(t, ok)
	require.Equal(t, clipboardImageType, gotMime)

	sel := f.readUntil(clDevice, deviceEvtSelection)
	sr := newArgReader(sel.args)
	selID, ok := sr.uint32()
	require.True(t, ok)
	require.Equal(t, offerID, selID)

	// Paste: offer.receive("image/png", writeFD); read from the pipe read end.
	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	defer pr.Close()

	var recv eventBody
	recv.string(clipboardImageType)
	f.req(offerID, offerReqReceive, recv.bytes(), int(pw.Fd()))
	require.NoError(t, pw.Close()) // drop our copy so the read end can EOF

	require.NoError(t, pr.SetReadDeadline(time.Now().Add(3*time.Second)))
	got, err := io.ReadAll(pr)
	require.NoError(t, err)
	require.Equal(t, png, got)
}

func TestPasteFlow_EmptyClipboardSendsNullSelection(t *testing.T) {
	f := newTestServer(t, stubClipboard{data: nil})
	f.handshake()

	// With no image, the server sends selection(null) and no data_offer.
	sel := f.readUntil(clDevice, deviceEvtSelection)
	sr := newArgReader(sel.args)
	selID, ok := sr.uint32()
	require.True(t, ok)
	require.Equal(t, uint32(0), selID, "selection must be null when clipboard is empty")
}

func TestGlobals_SeatAdvertisedAtV2(t *testing.T) {
	// wl-clipboard-rs ignores any wl_seat advertised below version 2, which
	// surfaces to the user as "There are no seats". Guard against a regression.
	f := newTestServer(t, stubClipboard{})
	var b eventBody
	b.uint32(clRegistry)
	f.req(displayID, displayReqGetRegistry, b.bytes())

	var seatVer uint32
	var sawSeat bool
	for i := 0; i < 2; i++ { // we advertise exactly two globals
		g := f.readUntil(clRegistry, registryEvtGlobal)
		r := newArgReader(g.args)
		_, ok := r.uint32() // name
		require.True(t, ok)
		iface, ok := r.string()
		require.True(t, ok)
		ver, ok := r.uint32()
		require.True(t, ok)
		if iface == ifaceSeat {
			sawSeat = true
			seatVer = ver
		}
	}
	require.True(t, sawSeat, "wl_seat global must be advertised")
	require.GreaterOrEqual(t, seatVer, uint32(2), "wl_seat must be >= v2 for wl-clipboard-rs")
}

func TestParseBuffered_RejectsUndersizeHeader(t *testing.T) {
	// A header claiming size < 8 can never advance the buffer; it must be a
	// hard error so readMessage closes the connection instead of spinning.
	c := &conn{}
	hdr := make([]byte, 8)
	order.PutUint32(hdr[0:4], 1)             // object id
	order.PutUint32(hdr[4:8], uint32(4)<<16) // size=4 (< 8), opcode=0
	c.buf = hdr

	_, ok, err := c.parseBuffered()
	require.False(t, ok)
	require.Error(t, err)
}

func TestWireString_RoundTrip(t *testing.T) {
	for _, s := range []string{"", "a", "wl_seat", "image/png", "zwlr_data_control_manager_v1"} {
		var b eventBody
		b.string(s)
		// Encoded length must be 4-byte aligned.
		require.Zero(t, len(b.bytes())%4, "string %q not padded to 4 bytes", s)
		got, ok := newArgReader(b.bytes()).string()
		require.True(t, ok)
		require.Equal(t, s, got)
	}
}

func TestWireSync_RepliesDoneAndDeleteID(t *testing.T) {
	f := newTestServer(t, stubClipboard{})
	const cb = 2
	var b eventBody
	b.uint32(cb)
	f.req(displayID, displayReqSync, b.bytes())

	done := f.readUntil(cb, callbackEvtDone)
	require.NotNil(t, done)
	del := f.readUntil(displayID, displayEvtDeleteID)
	dr := newArgReader(del.args)
	gotID, ok := dr.uint32()
	require.True(t, ok)
	require.Equal(t, uint32(cb), gotID)
}

// TestPasteFlow_UsesClientSession checks the session resolved for a connection
// reaches the host clipboard fetch, so a paste is read from the attach that
// asked for it rather than from whatever the bridge itself was started with.
func TestPasteFlow_UsesClientSession(t *testing.T) {
	var saw string
	png := []byte("\x89PNG\r\n\x1a\nHELLO")
	f := newTestServer(t, stubClipboard{data: png, sawSessionID: &saw},
		func(*net.UnixConn) (string, error) { return "sess-from-client", nil })
	f.handshake()
	f.readUntil(clDevice, deviceEvtSelection)

	require.Equal(t, "sess-from-client", saw)
}
