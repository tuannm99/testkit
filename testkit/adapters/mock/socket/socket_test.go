package socket

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
)

func TestWSAutoAckPongAndResetFault(t *testing.T) {
	hub := httpmock.NewHub(t.TempDir())
	s := NewServer(hub, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	if _, err := s.Configure("ns1", "partner", Config{AutoAck: true, Pong: true,
		Faults: []*Fault{{AfterMessages: 2, Action: "reset"}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ns/ns1/partner"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`))
	if _, m, _ := c.Read(ctx); string(m) != `{"type":"pong"}` {
		t.Fatalf("pong: %s", m)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"id":"m1","type":"order.paid"}`))
	if _, m, _ := c.Read(ctx); string(m) != `{"ack":"m1"}` {
		t.Fatalf("ack: %s", m)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"id":"m2","type":"order.paid"}`))
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected the connection to be reset after the 2nd message (no ack)")
	}
	// reconnect: the fault fired once, m2 is acked now
	c2, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.CloseNow()
	_ = c2.Write(ctx, websocket.MessageText, []byte(`{"id":"m2","type":"order.paid"}`))
	if _, m, _ := c2.Read(ctx); string(m) != `{"ack":"m2"}` {
		t.Fatalf("ack after reconnect: %s", m)
	}
	n := 0
	for _, e := range hub.Journal("ns1", "partner") {
		if e.Path == "message" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("journal messages = %d, want 3 (m1, m2, m2 again)", n)
	}
}

func TestTCPLength32FramingFragmentAndCoalesce(t *testing.T) {
	hub := httpmock.NewHub(t.TempDir())
	s := NewServer(hub, nil)
	port, err := s.Configure("ns1", "iso", Config{Protocol: "tcp", Framing: "length32", Script: []Action{
		{Expect: "^HELLO$"},
		{Fragment: true}, {Send: "WELCOME"},
		{Coalesce: []string{"A", "BB"}},
		{Close: "reset"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Reset("ns1")
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	frame := func(m string) []byte {
		b := make([]byte, 4+len(m))
		binary.BigEndian.PutUint32(b, uint32(len(m)))
		copy(b[4:], m)
		return b
	}
	_, _ = c.Write(frame("HELLO"))
	r := bufio.NewReader(c)
	read := func() string {
		var n uint32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, n)
		_, _ = io.ReadFull(r, b)
		return string(b)
	}
	if got := read(); got != "WELCOME" {
		t.Fatalf("fragmented frame reassembled as %q", got)
	}
	if a, b := read(), read(); a != "A" || b != "BB" {
		t.Fatalf("coalesced frames split as %q %q", a, b)
	}
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("expected reset")
	}
}

func TestWSHalfOpenIsSilent(t *testing.T) {
	hub := httpmock.NewHub(t.TempDir())
	s := NewServer(hub, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	_, _ = s.Configure("ns1", "p", Config{AutoAck: true, Pong: true, Faults: []*Fault{{AfterMessages: 1, Action: "half_open"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ns/ns1/p", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"id":"m1"}`))
	// The client can still write (no error) but never gets an ack or a pong.
	for i := 0; i < 3; i++ {
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
			t.Fatalf("write %d failed: half-open must not produce I/O errors: %v", i, err)
		}
	}
	rctx, rcancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer rcancel()
	if _, m, err := c.Read(rctx); err == nil {
		t.Fatalf("got %s from a half-open peer", m)
	} else if rctx.Err() == nil {
		t.Fatalf("read ended with an error instead of silence: %v", err)
	}
	s.Reset("ns1")
}
