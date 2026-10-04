// Package httpmock is the HTTP/webhook part of the Mock Hub.
//
// Data plane:    /ns/{ns}/{mock}/...            (what the service under test calls)
// Control plane: /_mock/ns/{ns}/...              (what TestKit calls)
//
// Every mock lives inside a run namespace, keeps a journal of what it
// received, can be scripted with response sequences and faults, validates
// requests against the OpenAPI document of the real API, and records the
// version/date of the API it imitates.
package httpmock

import (
	"encoding/json"
	"time"
)

// MockConfig is registered once per namespace and mock (at case start).
type MockConfig struct {
	Kind       string `json:"kind"`        // http | webhook
	OpenAPI    string `json:"openapi"`     // spec file name inside the hub spec dir
	APIVersion string `json:"api_version"` // version of the real API imitated
	VerifiedAt string `json:"verified_at"` // date the mock was verified against the real API
	// Idempotency: requests repeating a key already answered with 2xx get the
	// stored response back (the way real payment APIs behave).
	IdempotencyHeader string `json:"idempotency_header,omitempty"`
	// Signature: verify HMAC-SHA256 signatures of inbound requests.
	Signature *SignatureConfig `json:"signature,omitempty"`
	// Redact lists extra headers never written to the journal (Authorization,
	// Cookie and signature headers are always redacted).
	Redact []string `json:"redact,omitempty"`
}

// SignatureConfig: header value is "sha256=<hex(hmac_sha256(secret, body))>".
type SignatureConfig struct {
	Header string `json:"header"`
	Secret string `json:"secret"`
}

// Match selects requests a rule applies to. Empty fields match everything.
type Match struct {
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`      // exact or path.Match glob, relative to the mock base
	Operation string `json:"operation,omitempty"` // OpenAPI operationId
}

// Delay describes a response delay: fixed, or drawn from a distribution.
type Delay struct {
	Fixed  Duration `json:"fixed,omitempty"`
	Dist   string   `json:"dist,omitempty"` // uniform | normal | exponential
	Min    Duration `json:"min,omitempty"`
	Max    Duration `json:"max,omitempty"`
	Mean   Duration `json:"mean,omitempty"`
	StdDev Duration `json:"stddev,omitempty"`
}

// Fault kinds.
const (
	FaultReset = "reset" // TCP RST without a response
	FaultHang  = "hang"  // accept and never answer (until the client gives up)
	FaultClose = "close" // close the connection after headers, truncating the body
)

// Response is one scripted answer.
type Response struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`      // JSON body (placeholders allowed)
	BodyText   string            `json:"body_text,omitempty"` // raw body (wins over Body)
	RetryAfter string            `json:"retry_after,omitempty"`
	Delay      *Delay            `json:"delay,omitempty"`
	Fault      string            `json:"fault,omitempty"`
}

// Rule is a response sequence. Each matching request consumes the next
// response; the last one repeats once the sequence is exhausted.
type Rule struct {
	Match     Match      `json:"match"`
	Responses []Response `json:"responses"`
}

// Script replaces every rule of a mock.
type Script struct {
	Rules []Rule `json:"rules"`
}

// Entry is one journal line. Bodies are truncated and secrets redacted.
type Entry struct {
	Seq            int64             `json:"seq"`
	At             time.Time         `json:"at"`
	Mock           string            `json:"mock"`
	Direction      string            `json:"direction"` // in: called by the SUT; out: webhook sent to the SUT
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Query          string            `json:"query,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           string            `json:"body,omitempty"`
	Operation      string            `json:"operation,omitempty"`
	SchemaErrors   []string          `json:"schema_errors,omitempty"`
	SignatureValid *bool             `json:"signature_valid,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Replayed       bool              `json:"replayed,omitempty"`
	Rule           int               `json:"rule"` // index of the rule used, -1 default
	Status         int               `json:"status"`
	RespBody       string            `json:"response_body,omitempty"`
	Fault          string            `json:"fault,omitempty"`
	DelayMS        float64           `json:"delay_ms,omitempty"`
	Error          string            `json:"error,omitempty"`
	Unconfigured   bool              `json:"unconfigured,omitempty"`
}

// MockInfo is returned by the control API (provenance of every mock).
type MockInfo struct {
	Name   string     `json:"name"`
	Config MockConfig `json:"config"`
	Rules  int        `json:"rules"`
}

// WebhookRequest asks the hub to call the service under test (reverse direction).
type WebhookRequest struct {
	Mock    string            `json:"mock"`
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body"`
	// Sign: valid | invalid | missing (default valid when Secret is set).
	Sign   string   `json:"sign,omitempty"`
	Header string   `json:"header,omitempty"` // signature header (default X-Signature)
	Secret string   `json:"secret,omitempty"`
	Repeat int      `json:"repeat,omitempty"` // send N times (duplicates)
	Delay  Duration `json:"delay,omitempty"`  // wait before sending (late delivery)
}

// WebhookResult is the answer of the service under test.
type WebhookResult struct {
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
	Error  string `json:"error,omitempty"`
	Seq    int64  `json:"seq"`
}

// Duration marshals as "150ms".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		*d = Duration(time.Duration(n * float64(time.Millisecond)))
		return nil
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}
