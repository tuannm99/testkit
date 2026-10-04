package smtp

import (
	"context"
	"net"
	"net/smtp"
	"strings"
	"testing"

	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
)

func TestScriptedFailuresThenDelivery(t *testing.T) {
	hub := httpmock.NewHub(t.TempDir())
	s := NewServer(hub, "", nil)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx, l)
	ns := "tk_test_1"
	if err := s.SetScript(ns, []string{"451@data", "disconnect@data", "550@rcpt", "ok"}); err != nil {
		t.Fatal(err)
	}
	send := func() error {
		return smtp.SendMail(l.Addr().String(), nil, "orders+"+ns+"@shop.test", []string{"c@shop.test"},
			[]byte("Subject: hi\r\n\r\nbody\r\n"))
	}
	if err := send(); err == nil || !strings.Contains(err.Error(), "451") {
		t.Fatalf("1st: want 451, got %v", err)
	}
	if err := send(); err == nil {
		t.Fatal("2nd: want a dropped connection")
	}
	if err := send(); err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("3rd: want 550, got %v", err)
	}
	if err := send(); err != nil {
		t.Fatalf("4th: %v", err)
	}
	j := hub.Journal(ns, "smtp")
	var got []string
	for _, e := range j {
		got = append(got, e.Path)
	}
	want := "rejected at data with 451|disconnected in the middle of DATA|rejected at rcpt with 550|delivered"
	if strings.Join(got, "|") != want {
		t.Fatalf("journal %q\nwant     %q", strings.Join(got, "|"), want)
	}
	if j[3].Headers["Subject"] != "hi" || j[3].Headers["To"] != "c@shop.test" {
		t.Fatalf("%+v", j[3])
	}
}

func TestParseBehaviour(t *testing.T) {
	for _, s := range []string{"ok", "451", "451@data", "550@rcpt", "disconnect@data", "slow(2s)"} {
		if _, err := ParseBehaviour(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := ParseBehaviour("nope"); err == nil {
		t.Error("nope accepted")
	}
}
