// Package smtp is the SMTP part of the Mock Hub: an SMTP server that can
// answer with temporary (4xx) or permanent (5xx) errors, drop the connection
// in the middle of DATA, or answer slowly — then relays accepted messages to
// Mailpit so mail checks keep working.
//
// Namespace: taken from the envelope sender "local+<ns>@domain" (the service
// descriptor sets MAIL_FROM: orders+{{ .NS }}@...); otherwise "default".
// Script: one behaviour per SMTP session (one mail), the last one repeats:
//
//	ok | 451@data | 550@rcpt | disconnect@data | slow(2s)
package smtp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
)

// Behaviour of one session.
type Behaviour struct {
	Kind  string        // ok | code | disconnect | slow
	Code  int           // SMTP reply code for Kind=code
	Stage string        // mail | rcpt | data
	Delay time.Duration // for slow
	Raw   string
}

var behRe = regexp.MustCompile(`^(ok|disconnect|slow|\d{3})(\(([^)]*)\))?(@(mail|rcpt|data))?$`)

// ParseBehaviour parses the script shorthand.
func ParseBehaviour(s string) (Behaviour, error) {
	m := behRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Behaviour{}, fmt.Errorf("bad smtp behaviour %q (ok, 451@data, 550@rcpt, disconnect@data, slow(2s))", s)
	}
	b := Behaviour{Kind: m[1], Stage: m[5], Raw: s}
	if b.Stage == "" {
		b.Stage = "data"
	}
	switch {
	case m[1] == "slow":
		d, err := time.ParseDuration(m[3])
		if err != nil {
			return b, fmt.Errorf("slow(%s): %w", m[3], err)
		}
		b.Delay = d
	case m[1] != "ok" && m[1] != "disconnect":
		b.Kind = "code"
		b.Code, _ = strconv.Atoi(m[1])
	}
	return b, nil
}

// Server is the SMTP mock.
type Server struct {
	Hub   *httpmock.Hub
	Relay string // Mailpit SMTP address
	Log   *slog.Logger

	mu      sync.Mutex
	scripts map[string][]Behaviour
	next    map[string]int
}

func NewServer(hub *httpmock.Hub, relay string, log *slog.Logger) *Server {
	return &Server{Hub: hub, Relay: relay, Log: log, scripts: map[string][]Behaviour{}, next: map[string]int{}}
}

// SetScript replaces the behaviours of a namespace.
func (s *Server) SetScript(ns string, items []string) error {
	var bs []Behaviour
	for _, it := range items {
		b, err := ParseBehaviour(it)
		if err != nil {
			return err
		}
		bs = append(bs, b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[ns], s.next[ns] = bs, 0
	return nil
}

// Reset forgets a namespace.
func (s *Server) Reset(ns string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.scripts, ns)
	delete(s.next, ns)
}

func (s *Server) behaviour(ns string) Behaviour {
	s.mu.Lock()
	defer s.mu.Unlock()
	bs := s.scripts[ns]
	if len(bs) == 0 {
		return Behaviour{Kind: "ok", Stage: "data", Raw: "ok"}
	}
	i := s.next[ns]
	if i >= len(bs) {
		i = len(bs) - 1
	} else {
		s.next[ns]++
	}
	return bs[i]
}

var nsRe = regexp.MustCompile(`\+(tk_[a-z0-9_]+)@`)

// Serve accepts SMTP sessions until ctx is done.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.session(c)
	}
}

type session struct {
	from    string
	rcpt    []string
	ns      string
	data    []byte
	beh     Behaviour
	outcome string
	code    int
}

func (s *Server) session(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	reply := func(code int, msg string) {
		fmt.Fprintf(w, "%d %s\r\n", code, msg)
		_ = w.Flush()
	}
	reply(220, "mockhub ESMTP")
	ss := &session{ns: "default"}
	start := time.Now()
	defer func() {
		if ss.outcome == "" {
			return
		}
		s.record(ss, start)
	}()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if ss.outcome == "" && ss.from != "" {
				ss.outcome = "client closed: " + err.Error()
			}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			fmt.Fprintf(w, "250-mockhub\r\n250 8BITMIME\r\n")
			_ = w.Flush()
		case "MAIL":
			ss.from = addr(line)
			if m := nsRe.FindStringSubmatch(ss.from); m != nil {
				ss.ns = m[1]
			}
			ss.beh = s.behaviour(ss.ns)
			if ss.beh.Kind == "slow" {
				time.Sleep(ss.beh.Delay)
			}
			if s.fault(ss, "mail", c, reply) {
				return
			}
			reply(250, "OK")
		case "RCPT":
			ss.rcpt = append(ss.rcpt, addr(line))
			if s.fault(ss, "rcpt", c, reply) {
				return
			}
			reply(250, "OK")
		case "DATA":
			reply(354, "end with <CRLF>.<CRLF>")
			if ss.beh.Kind == "disconnect" && ss.beh.Stage == "data" {
				// read part of the message, then drop the connection mid-DATA
				buf := make([]byte, 64)
				_, _ = io.ReadAtLeast(r, buf, 1)
				ss.outcome, ss.code = "disconnected in the middle of DATA", 0
				return
			}
			data, err := readData(r)
			if err != nil {
				ss.outcome = "client closed during DATA"
				return
			}
			ss.data = data
			if s.fault(ss, "data", c, reply) {
				continue
			}
			if err := s.relay(ss); err != nil {
				ss.outcome, ss.code = "accepted but relay to Mailpit failed: "+err.Error(), 451
				reply(451, "relay failed")
				continue
			}
			ss.outcome, ss.code = "delivered", 250
			reply(250, "OK queued")
		case "RSET":
			ss.rcpt, ss.data = nil, nil
			reply(250, "OK")
		case "NOOP":
			reply(250, "OK")
		case "QUIT":
			reply(221, "bye")
			return
		default:
			reply(502, "command not implemented")
		}
	}
}

// fault applies the scripted error for the stage; true when it answered.
func (s *Server) fault(ss *session, stage string, c net.Conn, reply func(int, string)) bool {
	b := ss.beh
	if b.Stage != stage {
		return false
	}
	switch b.Kind {
	case "code":
		ss.outcome, ss.code = fmt.Sprintf("rejected at %s with %d", stage, b.Code), b.Code
		msg := "temporary failure, try again later"
		if b.Code >= 500 {
			msg = "permanent failure"
		}
		reply(b.Code, msg)
		return true
	case "disconnect":
		ss.outcome = "disconnected at " + stage
		_ = c.Close()
		return true
	}
	return false
}

func addr(line string) string {
	if i := strings.Index(line, "<"); i >= 0 {
		if j := strings.Index(line[i:], ">"); j > 0 {
			return line[i+1 : i+j]
		}
	}
	parts := strings.SplitN(line, ":", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func readData(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == ".\r\n" || line == ".\n" {
			return out, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		out = append(out, line...)
	}
}

func (s *Server) relay(ss *session) error {
	if s.Relay == "" {
		return nil
	}
	return smtp.SendMail(s.Relay, nil, ss.from, ss.rcpt, ss.data)
}

func (s *Server) record(ss *session, start time.Time) {
	subject := ""
	for _, l := range strings.Split(string(ss.data), "\n") {
		if strings.HasPrefix(strings.ToLower(l), "subject:") {
			subject = strings.TrimSpace(l[8:])
			break
		}
	}
	s.Hub.Record(ss.ns, httpmock.Entry{Mock: "smtp", Direction: "in", Method: "SMTP", Path: ss.outcome, Status: ss.code,
		Headers: map[string]string{"From": ss.from, "To": strings.Join(ss.rcpt, ","), "Subject": subject, "Behaviour": ss.beh.Raw},
		DelayMS: float64(time.Since(start)) / float64(time.Millisecond), Rule: -1})
}
