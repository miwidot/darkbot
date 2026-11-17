// Package obstest is a fake OBS (obs-websocket v5) for tests.
package obstest

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

const (
	// connBuffer: accepted connections kept for the test to inspect.
	connBuffer = 8
	// notReady is obs-websocket's request status NotReady.
	notReady = 207
)

// Server speaks just enough obs-websocket v5 (Hello, Identify,
// Identified, GetVersion) for the connection logic.
type Server struct {
	srv        *httptest.Server
	rejectAuth bool
	// Frozen stops answering requests (a hung OBS).
	Frozen atomic.Bool
	// Busy answers every request with NotReady (207), like OBS while it
	// loads a scene collection.
	Busy atomic.Bool
	// State is the fake OBS's scenes and sources.
	State *State
	// Conns receives each accepted connection.
	Conns chan *websocket.Conn
	wmu   sync.Mutex        // gorilla allows one concurrent writer
	live  []*websocket.Conn // guarded by wmu; cut by Close
}

func (f *Server) write(c *websocket.Conn, v any) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_ = c.WriteJSON(v)
}

// GoAway closes c like OBS does on exit.
func (f *Server) GoAway(c *websocket.Conn, code int, text string) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, text))
	c.Close()
}

// New starts a fake OBS on a free port. With rejectAuth it asks for a
// password and rejects any, like OBS with a wrong password (close 4009).
func New(t testing.TB, rejectAuth bool) *Server {
	return NewOn(t, "127.0.0.1:0", rejectAuth)
}

// NewOn starts a fake OBS on addr (to bring "OBS" back on the same port).
func NewOn(t testing.TB, addr string, rejectAuth bool) *Server {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	f := &Server{rejectAuth: rejectAuth, Conns: make(chan *websocket.Conn, connBuffer), State: NewState()}
	up := websocket.Upgrader{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.wmu.Lock()
		f.live = append(f.live, c)
		f.wmu.Unlock()
		select {
		case f.Conns <- c:
		default:
		}
		f.serve(c)
	}))
	f.srv.Listener.Close()
	f.srv.Listener = l
	f.srv.Start()
	t.Cleanup(f.Close)
	return f
}

type msg struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

func (f *Server) serve(c *websocket.Conn) {
	defer c.Close()
	hello := map[string]any{"obsWebSocketVersion": "5.7.6", "rpcVersion": 1}
	if f.rejectAuth {
		hello["authentication"] = map[string]string{"challenge": "c", "salt": "s"}
	}
	f.write(c, map[string]any{"op": 0, "d": hello})
	for {
		var m msg
		if err := c.ReadJSON(&m); err != nil {
			return
		}
		switch m.Op {
		case 1: // Identify
			if f.rejectAuth {
				f.GoAway(c, 4009, "Authentication failed.")
				return
			}
			f.write(c, map[string]any{"op": 2, "d": map[string]any{"negotiatedRpcVersion": 1}})
		case 6: // Request
			if f.Frozen.Load() {
				continue
			}
			var req struct {
				Type string          `json:"requestType"`
				ID   string          `json:"requestId"`
				Data json.RawMessage `json:"requestData"`
			}
			_ = json.Unmarshal(m.D, &req)
			if f.Busy.Load() {
				f.write(c, map[string]any{"op": 7, "d": map[string]any{
					"requestType": req.Type, "requestId": req.ID,
					"requestStatus": map[string]any{"result": false, "code": notReady, "comment": "OBS is not ready to perform the request."},
				}})
				continue
			}
			data, code := f.State.handle(req.Type, req.Data)
			status := map[string]any{"result": code == statusOK, "code": code}
			f.write(c, map[string]any{"op": 7, "d": map[string]any{
				"requestType": req.Type, "requestId": req.ID,
				"requestStatus": status, "responseData": data,
			}})
		}
	}
}

// Close stops the server and cuts its connections without a goodbye,
// like a crashing OBS. (httptest doesn't close upgraded connections.)
func (f *Server) Close() {
	f.wmu.Lock()
	for _, c := range f.live {
		_ = c.NetConn().Close()
	}
	f.live = nil
	f.wmu.Unlock()
	f.srv.Close()
}

// Addr is "host:port" of the server.
func (f *Server) Addr() string { return f.srv.Listener.Addr().String() }

// HostPort splits Addr.
func (f *Server) HostPort(t testing.TB) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(f.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	return host, p
}
