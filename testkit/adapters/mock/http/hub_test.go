package httpmock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func specDir(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "mocks")
}

func newTestHub(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()
	h := NewHub(specDir(t))
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return srv, NewClient(srv.URL)
}

func charge(t *testing.T, base, ns, key, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, DataURL(base, ns, "payment")+"/v1/charges", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestScriptSequenceIdempotencyAndJournal(t *testing.T) {
	srv, c := newTestHub(t)
	ctx := context.Background()
	if err := c.Configure(ctx, "ns1", "payment", MockConfig{Kind: "http", OpenAPI: "payment.openapi.yaml",
		APIVersion: "2024-06-01", VerifiedAt: "2026-10-01", IdempotencyHeader: "Idempotency-Key"}); err != nil {
		t.Fatal(err)
	}
	err := c.Script(ctx, "ns1", "payment", Script{Rules: []Rule{{
		Match: Match{Operation: "createCharge"},
		Responses: []Response{
			{Status: 500},
			{Status: 429, RetryAfter: "2"},
			{Status: 201, Body: json.RawMessage(`{"id":"ch_${req.body.order_id}","status":"succeeded"}`)},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"order_id":"o1","amount":100,"currency":"USD"}`
	want := []int{500, 429, 201}
	for i, code := range want {
		resp, b := charge(t, srv.URL, "ns1", "order-o1", body)
		if resp.StatusCode != code {
			t.Fatalf("call %d: status %d want %d (%s)", i, resp.StatusCode, code, b)
		}
		if code == 429 && resp.Header.Get("Retry-After") != "2" {
			t.Fatalf("missing Retry-After")
		}
		if code == 201 && !strings.Contains(b, `"ch_o1"`) {
			t.Fatalf("placeholder not rendered: %s", b)
		}
	}
	// Same key after success: stored response is replayed, script not consumed.
	resp, b := charge(t, srv.URL, "ns1", "order-o1", body)
	if resp.StatusCode != 201 || !strings.Contains(b, "ch_o1") {
		t.Fatalf("replay: %d %s", resp.StatusCode, b)
	}
	j, err := c.Journal(ctx, "ns1", "payment")
	if err != nil {
		t.Fatal(err)
	}
	if len(j) != 4 || !j[3].Replayed || j[0].Operation != "createCharge" {
		t.Fatalf("journal: %+v", j)
	}
	if j[0].Headers["Authorization"] != "[REDACTED]" {
		t.Fatalf("authorization not redacted: %v", j[0].Headers)
	}
	for _, e := range j {
		if len(e.SchemaErrors) != 0 {
			t.Fatalf("unexpected schema errors: %v", e.SchemaErrors)
		}
	}
	// Other namespaces are isolated.
	if j2, _ := c.Journal(ctx, "ns2", ""); len(j2) != 0 {
		t.Fatalf("namespace leak: %+v", j2)
	}
}

func TestSchemaValidationFlagsBadRequests(t *testing.T) {
	srv, c := newTestHub(t)
	ctx := context.Background()
	_ = c.Configure(ctx, "ns1", "payment", MockConfig{Kind: "http", OpenAPI: "payment.openapi.yaml", APIVersion: "x", VerifiedAt: "y"})
	// missing Idempotency-Key, wrong currency, unknown field
	charge(t, srv.URL, "ns1", "", `{"order_id":"o1","amount":100,"currency":"XXX","extra":1}`)
	j, _ := c.Journal(ctx, "ns1", "payment")
	if len(j) != 1 || len(j[0].SchemaErrors) < 2 {
		t.Fatalf("expected schema errors, got %+v", j)
	}
	// default response comes from the OpenAPI example
	resp, b := charge(t, srv.URL, "ns1", "order-o2-key", `{"order_id":"o2","amount":1,"currency":"EUR"}`)
	if resp.StatusCode != 201 || !strings.Contains(b, "ch_o2") {
		t.Fatalf("default example: %d %s", resp.StatusCode, b)
	}
}

func TestFaultResetAndDelay(t *testing.T) {
	srv, c := newTestHub(t)
	ctx := context.Background()
	_ = c.Configure(ctx, "ns1", "payment", MockConfig{Kind: "http"})
	_ = c.Script(ctx, "ns1", "payment", Script{Rules: []Rule{{Responses: []Response{
		{Fault: FaultReset},
		{Status: 200, Delay: &Delay{Fixed: Duration(150 * time.Millisecond)}},
	}}}})
	_, err := http.Post(DataURL(srv.URL, "ns1", "payment")+"/x", "application/json", bytes.NewReader([]byte(`{}`)))
	if err == nil {
		t.Fatal("expected connection reset")
	}
	start := time.Now()
	resp, err := http.Post(DataURL(srv.URL, "ns1", "payment")+"/x", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil || resp.StatusCode != 200 || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("delay: %v %v %v", err, resp, time.Since(start))
	}
}

func TestWebhookSigning(t *testing.T) {
	var got []string
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Signature") == Sign("s3cret", b) {
			got = append(got, "valid")
			w.WriteHeader(200)
		} else {
			got = append(got, "invalid")
			w.WriteHeader(401)
		}
	}))
	defer sut.Close()
	_, c := newTestHub(t)
	ctx := context.Background()
	res, err := c.SendWebhook(ctx, "ns1", WebhookRequest{Mock: "psp", URL: sut.URL, Body: json.RawMessage(`{"a":1}`), Secret: "s3cret", Repeat: 2})
	if err != nil || len(res) != 2 || res[1].Status != 200 {
		t.Fatalf("valid: %v %+v", err, res)
	}
	res, _ = c.SendWebhook(ctx, "ns1", WebhookRequest{Mock: "psp", URL: sut.URL, Body: json.RawMessage(`{"a":1}`), Secret: "s3cret", Sign: "invalid"})
	if res[0].Status != 401 || strings.Join(got, ",") != "valid,valid,invalid" {
		t.Fatalf("invalid: %+v %v", res, got)
	}
	j, _ := c.Journal(ctx, "ns1", "psp")
	if len(j) != 3 || j[0].Direction != "out" || !strings.HasSuffix(j[0].Headers["X-Signature"], "[REDACTED]") {
		t.Fatalf("journal: %+v", j)
	}
}
