// Package socket is the WebSocket/TCP part of the Mock Hub: a partner that
// acks messages, answers heartbeats, and can misbehave on purpose — close or
// reset the connection, go silent while keeping it open (half-open), drop
// acks, split or merge frames, stop reading (backpressure). Every message it
// receives is journaled so tests can prove "no loss, no duplicate" across
// reconnects.
//
// WebSocket: ws://mockhub:8082/ns/{ns}/{mock}  (JSON text messages)
// TCP:       a listener allocated per namespace/mock (line or 4-byte length framing)
package socket

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
)

// Fault fires once, when the session has received AfterMessages data messages.
type Fault struct {
	AfterMessages int    `json:"after_messages"`
	Action        string `json:"action"`          // close | reset | half_open | drop_acks
	Count         int    `json:"count,omitempty"` // drop_acks: how many acks to drop
	fired         bool
}

// Action is one step of a scripted TCP/WS conversation.
type Action struct {
	Expect   string            `json:"expect,omitempty"`   // regexp the next received message must match
	Send     string            `json:"send,omitempty"`     // message to send
	Wait     httpmock.Duration `json:"wait,omitempty"`     // pause (server side)
	Close    string            `json:"close,omitempty"`    // normal | reset
	Fragment bool              `json:"fragment,omitempty"` // send the next message byte by byte (TCP) / in fragments (WS)
	Coalesce []string          `json:"coalesce,omitempty"` // several messages in one write (TCP)
	Stall    httpmock.Duration `json:"stall,omitempty"`    // stop reading for a while (backpressure)
}

// Config of one socket mock in one namespace.
type Config struct {
	Protocol string   `json:"protocol"`          // ws | tcp
	AutoAck  bool     `json:"auto_ack"`          // reply {"ack": <id>} to messages carrying an id
	Pong     bool     `json:"pong"`              // reply {"type":"pong"} to {"type":"ping"}
	Faults   []*Fault `json:"faults,omitempty"`  // misbehaviours (fire once each)
	Framing  string   `json:"framing,omitempty"` // tcp: line | length32
	Script   []Action `json:"script,omitempty"`  // tcp/ws scripted conversation (instead of auto mode)
}

type mockState struct {
	cfg      Config
	sessions int
	tcp      net.Listener
	done     chan struct{} // closed when the namespace is reset
}

// Server holds socket mocks of every namespace.
type Server struct {
	Hub *httpmock.Hub
	Log *slog.Logger

	mu    sync.Mutex
	mocks map[string]*mockState // ns/mock
}

func NewServer(hub *httpmock.Hub, log *slog.Logger) *Server {
	return &Server{Hub: hub, Log: log, mocks: map[string]*mockState{}}
}

func key(ns, mock string) string { return ns + "/" + mock }

// Configure registers or replaces a socket mock. For TCP it opens a
// listener and returns its port (the service connects to mockhub:<port>).
func (s *Server) Configure(ns, mock string, cfg Config) (int, error) {
	if cfg.Protocol == "" {
		cfg.Protocol = "ws"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.mocks[key(ns, mock)]
	if !ok {
		st = &mockState{done: make(chan struct{})}
		s.mocks[key(ns, mock)] = st
	}
	st.cfg = cfg
	if cfg.Protocol == "tcp" && st.tcp == nil {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			return 0, err
		}
		st.tcp = l
		go s.serveTCP(ns, mock, l)
	}
	if st.tcp != nil {
		return st.tcp.Addr().(*net.TCPAddr).Port, nil
	}
	return 0, nil
}

// Reset closes and forgets every socket mock of a namespace.
func (s *Server) Reset(ns string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, st := range s.mocks {
		if strings.HasPrefix(k, ns+"/") {
			if st.tcp != nil {
				st.tcp.Close()
			}
			close(st.done)
			delete(s.mocks, k)
		}
	}
}

func (s *Server) state(ns, mock string) (*mockState, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.mocks[key(ns, mock)]
	if !ok {
		return nil, 0
	}
	st.sessions++
	return st, st.sessions
}

func (s *Server) record(ns, mock string, sess int, path, body string, extra map[string]string) {
	h := map[string]string{"session": fmt.Sprint(sess)}
	for k, v := range extra {
		h[k] = v
	}
	dir := "in"
	if strings.HasPrefix(path, "sent") {
		dir = "out"
	}
	s.Hub.Record(ns, httpmock.Entry{Mock: mock, Direction: dir, Method: "SOCKET", Path: path, Body: body, Headers: h, Rule: -1})
}

// fault returns the fault to apply after n received messages (fires once).
func (s *Server) fault(st *mockState, n int) *Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range st.cfg.Faults {
		if !f.fired && n >= f.AfterMessages {
			f.fired = true
			return f
		}
	}
	return nil
}

var wsPath = regexp.MustCompile(`^/ns/([a-z0-9][a-z0-9_-]{0,62})/([A-Za-z0-9_-]+)$`)

// ServeHTTP accepts WebSocket sessions.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		_, _ = w.Write([]byte("ok"))
		return
	}
	m := wsPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, "expected /ns/{ns}/{mock}", http.StatusNotFound)
		return
	}
	ns, mock := m[1], m[2]
	st, sess := s.state(ns, mock)
	if st == nil {
		s.record(ns, mock, 0, "rejected: not configured", "", nil)
		http.Error(w, "socket mock not configured", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	s.record(ns, mock, sess, "connect", "", nil)
	reason := s.wsSession(r.Context(), conn, st, ns, mock, sess)
	s.record(ns, mock, sess, "disconnect", reason, nil)
}

type msg struct {
	ID   any    `json:"id"`
	Type string `json:"type"`
}

func (s *Server) wsSession(ctx context.Context, conn *websocket.Conn, st *mockState, ns, mock string, sess int) string {
	defer conn.CloseNow()
	received := 0
	dropAcks := 0
	if len(st.cfg.Script) > 0 {
		return s.wsScript(ctx, conn, st, ns, mock, sess)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return "read: " + err.Error()
		}
		var m msg
		_ = json.Unmarshal(data, &m)
		if m.Type == "ping" {
			if st.cfg.Pong {
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"pong"}`))
			}
			continue
		}
		received++
		s.record(ns, mock, sess, "message", string(data), map[string]string{"type": m.Type, "id": idString(m.ID)})
		if f := s.fault(st, received); f != nil {
			s.record(ns, mock, sess, "fault: "+f.Action, "", nil)
			switch f.Action {
			case "close":
				conn.Close(websocket.StatusGoingAway, "mock closes")
				return "fault close (no ack for the last message)"
			case "reset":
				return "fault reset (connection dropped, no close frame)"
			case "half_open":
				// Stay connected, never read nor answer again: no I/O error ever
				// reaches the client, only silence (no pong, no ack). Ends when
				// the namespace is reset or after 5 minutes.
				select {
				case <-st.done:
				case <-ctx.Done():
				case <-time.After(5 * time.Minute):
				}
				return "fault half-open (silent) until reset"
			case "drop_acks":
				dropAcks = max(f.Count, 1)
			}
		}
		if st.cfg.AutoAck && m.ID != nil {
			if dropAcks > 0 {
				dropAcks--
				s.record(ns, mock, sess, "ack dropped", idString(m.ID), nil)
				continue
			}
			ack, _ := json.Marshal(map[string]any{"ack": m.ID})
			if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
				return "write: " + err.Error()
			}
			s.record(ns, mock, sess, "sent ack", idString(m.ID), map[string]string{"id": idString(m.ID)})
		}
	}
}

func (s *Server) wsScript(ctx context.Context, conn *websocket.Conn, st *mockState, ns, mock string, sess int) string {
	for i, a := range st.cfg.Script {
		switch {
		case a.Expect != "":
			_, data, err := conn.Read(ctx)
			if err != nil {
				return fmt.Sprintf("step %d expect: %v", i+1, err)
			}
			ok, _ := regexp.MatchString(a.Expect, string(data))
			s.record(ns, mock, sess, "message", string(data), map[string]string{"expect_ok": fmt.Sprint(ok)})
			if !ok {
				return fmt.Sprintf("step %d: %q does not match %q", i+1, data, a.Expect)
			}
		case a.Send != "":
			var err error
			if a.Fragment {
				w, werr := conn.Writer(ctx, websocket.MessageText)
				if werr != nil {
					return werr.Error()
				}
				for _, b := range []byte(a.Send) {
					if _, err = w.Write([]byte{b}); err != nil {
						break
					}
				}
				err = w.Close()
			} else {
				err = conn.Write(ctx, websocket.MessageText, []byte(a.Send))
			}
			if err != nil {
				return err.Error()
			}
			s.record(ns, mock, sess, "sent", a.Send, nil)
		case a.Wait > 0:
			time.Sleep(time.Duration(a.Wait))
		case a.Close == "normal":
			conn.Close(websocket.StatusNormalClosure, "script")
			return "script close"
		case a.Close == "reset":
			return "script reset"
		}
	}
	<-ctx.Done()
	return "script done"
}

func idString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// ---------------------------------------------------------------------- TCP

func (s *Server) serveTCP(ns, mock string, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		st, sess := s.state(ns, mock)
		if st == nil {
			c.Close()
			return
		}
		go func() {
			s.record(ns, mock, sess, "connect", "", nil)
			reason := s.tcpSession(c, st, ns, mock, sess)
			s.record(ns, mock, sess, "disconnect", reason, nil)
		}()
	}
}

type framer struct {
	kind string
	r    *bufio.Reader
	c    net.Conn
}

func (f *framer) read() (string, error) {
	if f.kind == "length32" {
		var n uint32
		if err := binary.Read(f.r, binary.BigEndian, &n); err != nil {
			return "", err
		}
		if n > 1<<20 {
			return "", fmt.Errorf("frame of %d bytes", n)
		}
		buf := make([]byte, n)
		_, err := io.ReadFull(f.r, buf)
		return string(buf), err
	}
	line, err := f.r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func (f *framer) encode(m string) []byte {
	if f.kind == "length32" {
		b := make([]byte, 4+len(m))
		binary.BigEndian.PutUint32(b, uint32(len(m)))
		copy(b[4:], m)
		return b
	}
	return []byte(m + "\n")
}

func (s *Server) tcpSession(c net.Conn, st *mockState, ns, mock string, sess int) string {
	defer c.Close()
	f := &framer{kind: st.cfg.Framing, r: bufio.NewReader(c), c: c}
	fragmentNext := false
	for i, a := range st.cfg.Script {
		switch {
		case a.Stall > 0:
			// Backpressure: stop reading; the client's writes fill the buffers.
			s.record(ns, mock, sess, "stall", time.Duration(a.Stall).String(), nil)
			time.Sleep(time.Duration(a.Stall))
		case a.Expect != "":
			m, err := f.read()
			if err != nil {
				return fmt.Sprintf("step %d expect: %v", i+1, err)
			}
			ok, _ := regexp.MatchString(a.Expect, m)
			s.record(ns, mock, sess, "message", m, map[string]string{"expect_ok": fmt.Sprint(ok)})
			if !ok {
				return fmt.Sprintf("step %d: %q does not match %q", i+1, m, a.Expect)
			}
		case a.Fragment:
			fragmentNext = true
		case a.Send != "":
			b := f.encode(a.Send)
			if fragmentNext {
				for _, x := range b {
					if _, err := c.Write([]byte{x}); err != nil {
						return err.Error()
					}
					time.Sleep(time.Millisecond)
				}
				fragmentNext = false
			} else if _, err := c.Write(b); err != nil {
				return err.Error()
			}
			s.record(ns, mock, sess, "sent", a.Send, nil)
		case len(a.Coalesce) > 0:
			var b []byte
			for _, m := range a.Coalesce {
				b = append(b, f.encode(m)...)
			}
			if _, err := c.Write(b); err != nil {
				return err.Error()
			}
			s.record(ns, mock, sess, "sent coalesced", strings.Join(a.Coalesce, " | "), nil)
		case a.Wait > 0:
			time.Sleep(time.Duration(a.Wait))
		case a.Close == "reset":
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			return "script reset (RST)"
		case a.Close == "normal":
			return "script close"
		}
	}
	// After the script: echo-less drain until the client closes.
	_, _ = io.Copy(io.Discard, c)
	return "client closed"
}
