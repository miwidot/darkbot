package obs

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/miwidot/darkbot/internal/obs/obstest"
)

func waitDrop(t *testing.T, drops chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-drops:
		return err
	case <-time.After(within):
		t.Fatalf("the drop was not noticed within %s", within)
		return nil
	}
}

func TestConnectAndNoticeClose(t *testing.T) {
	f := obstest.New(t, false)
	drops := make(chan error, 1)
	o := New(func(err error) { drops <- err })
	host, port := f.HostPort(t)
	v, err := o.Connect(host, port, "")
	if err != nil || v != "OBS 33.0.1, websocket 5.7.6" || !o.Connected() {
		t.Fatalf("connect: %q %v", v, err)
	}
	// OBS closes (sends a close frame, as on exit).
	f.GoAway(<-f.Conns, websocket.CloseGoingAway, "bye")
	waitDrop(t, drops, 2*time.Second)
	if o.Connected() {
		t.Fatal("must show disconnected")
	}
}

func TestFrozenOBSIsDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a ping timeout")
	}
	f := obstest.New(t, false)
	drops := make(chan error, 1)
	o := New(func(err error) { drops <- err })
	host, port := f.HostPort(t)
	if _, err := o.Connect(host, port, ""); err != nil {
		t.Fatal(err)
	}
	f.Frozen.Store(true)
	waitDrop(t, drops, DropWithin+2*time.Second)
}

func TestWrongPassword(t *testing.T) {
	f := obstest.New(t, true)
	o := New(func(error) { t.Error("a failed connect is no drop") })
	host, port := f.HostPort(t)
	if _, err := o.Connect(host, port, "wrong"); !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}

func TestDisconnectIsNoDrop(t *testing.T) {
	f := obstest.New(t, false)
	drops := make(chan error, 1)
	o := New(func(err error) { drops <- err })
	host, port := f.HostPort(t)
	if _, err := o.Connect(host, port, ""); err != nil {
		t.Fatal(err)
	}
	o.Disconnect()
	select {
	case err := <-drops:
		t.Fatalf("Disconnect reported a drop: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestOBSNotRunning(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // nothing listens there now
	o := New(nil)
	if _, err := o.Connect("127.0.0.1", port, ""); err == nil || errors.Is(err, ErrAuth) {
		t.Fatalf("got %v", err)
	}
}

// OBS answering "NotReady" (busy loading, about to quit) is alive: no drop.
func TestBusyOBSIsNoDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a ping")
	}
	f := obstest.New(t, false)
	drops := make(chan error, 1)
	o := New(func(err error) { drops <- err })
	host, port := f.HostPort(t)
	if _, err := o.Connect(host, port, ""); err != nil {
		t.Fatal(err)
	}
	f.Busy.Store(true)
	select {
	case err := <-drops:
		t.Fatalf("a busy OBS was dropped: %v", err)
	case <-time.After(PingEvery + time.Second):
	}
	if !o.Connected() {
		t.Fatal("must stay connected")
	}
}
