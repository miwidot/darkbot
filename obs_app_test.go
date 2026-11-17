package main

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/miwidot/darkbot/internal/applog"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/obs"
	"github.com/miwidot/darkbot/internal/obs/obstest"
	"github.com/miwidot/darkbot/internal/stats"
)

// freePort returns a port nothing listens on (OBS not started yet).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newOBSApp(t *testing.T, port int) *App {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) { c.OBS.Host, c.OBS.Port = "127.0.0.1", port }); err != nil {
		t.Fatal(err)
	}
	log := applog.New()
	a := &App{cfg: cfg, log: log, st: stats.Open(filepath.Join(t.TempDir(), "s.json"), log)}
	a.obs = obs.New(a.onOBSDrop)
	t.Cleanup(a.obs.Disconnect)
	return a
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// OBS starts after darkbot, closes, and comes back: darkbot follows.
func TestOBSKeepAlive(t *testing.T) {
	port := freePort(t)
	a := newOBSApp(t, port)
	if err := a.ConnectOBS(); err == nil {
		t.Fatal("OBS isn't running yet")
	}
	if waiting, _ := a.obsStatus(); !waiting {
		t.Fatal("must wait for OBS")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	f := obstest.NewOn(t, addr, false)
	a.tryOBS() // the keep-alive loop's next round
	if !a.obs.Connected() {
		t.Fatal("must connect once OBS runs")
	}
	// OBS quits normally: it says goodbye, darkbot notices at once.
	f.GoAway(<-f.Conns, websocket.CloseGoingAway, "OBS is closing")
	f.Close()
	waitFor(t, "the drop", time.Second, func() bool { return !a.obs.Connected() })
	if waiting, _ := a.obsStatus(); !waiting {
		t.Fatal("after a drop darkbot must wait for OBS again")
	}
	f = obstest.NewOn(t, addr, false) // OBS is back
	a.tryOBS()
	if !a.obs.Connected() {
		t.Fatal("must reconnect")
	}
	// OBS crashes: the connection just ends without goodbye. The ping
	// notices it, at the latest after obs.DropWithin.
	f.Close()
	if testing.Short() {
		return
	}
	waitFor(t, "the crash", obs.DropWithin+2*time.Second, func() bool { return !a.obs.Connected() })
	obstest.NewOn(t, addr, false)
	a.tryOBS()
	if !a.obs.Connected() {
		t.Fatal("must reconnect after a crash")
	}
	a.DisconnectOBS()
	if waiting, _ := a.obsStatus(); waiting || a.obs.Connected() {
		t.Fatal("Disconnect must stop waiting")
	}
}

func TestOBSWrongPasswordStopsRetrying(t *testing.T) {
	f := obstest.New(t, true)
	_, port := f.HostPort(t)
	a := newOBSApp(t, port)
	if err := a.ConnectOBS(); err == nil {
		t.Fatal("password must be rejected")
	}
	if waiting, authFailed := a.obsStatus(); !authFailed || waiting {
		t.Fatalf("rejected password: no retries, say so (waiting=%v authFailed=%v)", waiting, authFailed)
	}
}
