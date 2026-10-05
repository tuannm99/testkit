package ai

import (
	"strings"
	"testing"
)

// fakeKey is a random-looking API key built at run time, so that no
// secret-shaped literal sits in the source.
var fakeKey = strings.Join([]string{"tk", "fake", "9fQ2xZr8LmT4vB7nK1pW3yD6hJ0sA5cE"}, "_")

func TestRedact(t *testing.T) {
	secrets := map[string]string{"TK_POSTGRES_PASSWORD": "tk-pg-test-only-7f3a"}
	in := strings.Join([]string{
		`DATABASE_URL=postgres://testkit:tk-pg-test-only-7f3a@postgres:5432/tk_20261005ui1_1?sslmode=disable`,
		`mail to cust-o1+tk_20261005ui1_1@shop.test and again CUST-O1+tk_20261005ui1_1@shop.test, other a.b@x.vn`,
		`{"Authorization": "Bearer abcdefghijklmnopqrstuvwxyz012345", "password": "hunter2hunter2", "card": "4111111111111111"}`,
		`X-Signature: sha256=9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08`,
		`phone +84 912 345 678, cccd 001203004567, from 10.0.3.7 to 127.0.0.1`,
		`jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U`,
		`run r20261005-024858-p6 TC-ORDER-017[kafka] at 2026-10-05T02:48:59Z took 1.5s, A1 expected eq paid got failed`,
		`files TC-PERF-001/kafka/assertions/BASE-p95_ms.json TC-UI-001/kafka/output/ui/order-status/artifacts/orders-order-status-trang--c8bc3-x/test-finished-1.png grafana/panel-payment_calls_total.a.prom.json`,
		"key " + fakeKey + " and api 3f6b2a9c8d7e1f0a4b5c6d7e8f9a0b1c",
	}, "\n")
	r := NewRedactor(secrets)
	out := r.Text(in)
	for _, leak := range []string{"tk-pg-test-only-7f3a", "cust-o1", "a.b@x.vn", "abcdefghijklmnop", "hunter2hunter2", "4111111111111111",
		"9f86d081884c7d659a2f", fakeKey, "3f6b2a9c8d7e1f0a4b5c6d7e8f9a0b1c", "912 345 678", "001203004567", "10.0.3.7", "eyJhbGciOi"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in:\n%s", leak, out)
		}
	}
	// What the model needs stays readable.
	for _, keep := range []string{"r20261005-024858-p6", "TC-ORDER-017[kafka]", "2026-10-05T02:48:59Z", "expected eq paid got failed",
		"postgres:5432/tk_20261005ui1_1", "127.0.0.1", "TC-PERF-001/kafka/assertions/BASE-p95_ms.json",
		"orders-order-status-trang--c8bc3-x/test-finished-1.png", "grafana/panel-payment_calls_total.a.prom.json"} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q in:\n%s", keep, out)
		}
	}
	// Same address (case-insensitive) → same placeholder.
	if strings.Count(out, "<email-1>") != 2 || !strings.Contains(out, "<email-2>") {
		t.Errorf("placeholders not stable:\n%s", out)
	}
	if err := Guard(out, secrets); err != nil {
		t.Errorf("guard rejects redacted text: %v\n%s", err, out)
	}
	if c := r.Counts(); c["email"] != 3 || c["secret"] != 1 {
		t.Errorf("counts %v", c)
	}
}

func TestGuardRefusesLeftovers(t *testing.T) {
	secrets := map[string]string{"S": "tk-pg-test-only-7f3a"}
	for _, payload := range []string{"pw tk-pg-test-only-7f3a", "mail x@y.com", "card 4111111111111111", "Authorization: Bearer abcdefghijkl"} {
		if Guard(payload, secrets) == nil {
			t.Errorf("guard accepted %q", payload)
		}
	}
}
