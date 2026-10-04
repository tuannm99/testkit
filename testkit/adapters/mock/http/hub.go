package httpmock

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

const maxBody = 1 << 20

// Hub is the in-memory state of the Mock Hub: namespaces -> mocks + journal.
// Other mock kinds (smtp, socket) record into the same journal.
type Hub struct {
	mu      sync.Mutex
	ns      map[string]*namespace
	specDir string
	specs   map[string]*spec
	rnd     *rand.Rand
	now     func() time.Time
	client  *http.Client
}

type spec struct {
	doc    *openapi3.T
	router routers.Router
}

type namespace struct {
	mocks   map[string]*mock
	journal []Entry
	seq     int64
}

type mock struct {
	name  string
	cfg   MockConfig
	rules []*ruleState
	idem  map[string]storedResp
	spec  *spec
}

type ruleState struct {
	Rule
	next int
}

type storedResp struct {
	status  int
	headers map[string]string
	body    []byte
}

// NewHub creates a hub reading OpenAPI documents from specDir.
func NewHub(specDir string) *Hub {
	return &Hub{
		ns:      map[string]*namespace{},
		specDir: specDir,
		specs:   map[string]*spec{},
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
		now:     time.Now,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *Hub) nsLocked(name string) *namespace {
	n, ok := h.ns[name]
	if !ok {
		n = &namespace{mocks: map[string]*mock{}}
		h.ns[name] = n
	}
	return n
}

// Record appends an entry to a namespace journal and returns its sequence.
func (h *Hub) Record(ns string, e Entry) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nsLocked(ns)
	n.seq++
	e.Seq = n.seq
	if e.At.IsZero() {
		e.At = h.now().UTC()
	}
	n.journal = append(n.journal, e)
	return e.Seq
}

// update modifies a journal entry in place (in-flight -> answered).
func (h *Hub) update(ns string, seq int64, fn func(*Entry)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.ns[ns]
	if !ok {
		return
	}
	for i := len(n.journal) - 1; i >= 0; i-- {
		if n.journal[i].Seq == seq {
			fn(&n.journal[i])
			n.journal[i].InFlight = false
			n.journal[i].DoneAt = h.now().UTC()
			return
		}
	}
}

func (h *Hub) loadSpec(name string) (*spec, error) {
	if s, ok := h.specs[name]; ok {
		return s, nil
	}
	file := filepath.Join(h.specDir, filepath.Clean("/"+name))
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(file)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", name, err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("invalid openapi %s: %w", name, err)
	}
	// Route on paths only: the mock base path is stripped before routing.
	doc.Servers = openapi3.Servers{{URL: "/"}}
	r, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, err
	}
	s := &spec{doc: doc, router: r}
	h.specs[name] = s
	return s, nil
}

// Configure registers (or replaces) a mock in a namespace and clears its script.
func (h *Hub) Configure(ns, name string, cfg MockConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := &mock{name: name, cfg: cfg, idem: map[string]storedResp{}}
	if cfg.OpenAPI != "" {
		s, err := h.loadSpec(cfg.OpenAPI)
		if err != nil {
			return err
		}
		m.spec = s
	}
	h.nsLocked(ns).mocks[name] = m
	return nil
}

// SetScript replaces the rules of a mock.
func (h *Hub) SetScript(ns, name string, s Script) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.nsLocked(ns).mocks[name]
	if !ok {
		return fmt.Errorf("mock %q not configured in namespace %q", name, ns)
	}
	m.rules = nil
	for _, r := range s.Rules {
		if len(r.Responses) == 0 {
			return fmt.Errorf("rule %+v has no responses", r.Match)
		}
		if r.Match.Operation != "" && m.spec == nil {
			return fmt.Errorf("rule matches operation %q but mock %q has no openapi", r.Match.Operation, name)
		}
		m.rules = append(m.rules, &ruleState{Rule: r})
	}
	return nil
}

// Journal returns a copy of the namespace journal (optionally one mock).
func (h *Hub) Journal(ns, mockName string) []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.ns[ns]
	if !ok {
		return []Entry{}
	}
	out := make([]Entry, 0, len(n.journal))
	for _, e := range n.journal {
		if mockName == "" || e.Mock == mockName {
			out = append(out, e)
		}
	}
	return out
}

// Mocks lists configured mocks (provenance for the evidence bundle).
func (h *Hub) Mocks(ns string) []MockInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []MockInfo
	if n, ok := h.ns[ns]; ok {
		for name, m := range n.mocks {
			out = append(out, MockInfo{Name: name, Config: m.cfg, Rules: len(m.rules)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Reset drops a namespace entirely.
func (h *Hub) Reset(ns string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.ns, ns)
}

// Namespaces lists active namespaces.
func (h *Hub) Namespaces() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for k := range h.ns {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var alwaysRedacted = []string{"authorization", "cookie", "set-cookie", "x-api-key", "proxy-authorization"}

func redactHeaders(hdr http.Header, extra []string, sigHeader string) map[string]string {
	red := map[string]bool{}
	for _, k := range alwaysRedacted {
		red[k] = true
	}
	for _, k := range extra {
		red[strings.ToLower(k)] = true
	}
	out := map[string]string{}
	for k, v := range hdr {
		lk := strings.ToLower(k)
		switch {
		case red[lk]:
			out[k] = "[REDACTED]"
		case sigHeader != "" && strings.EqualFold(k, sigHeader):
			// keep the scheme prefix, hide the value
			if s, _, ok := strings.Cut(strings.Join(v, ","), "="); ok {
				out[k] = s + "=[REDACTED]"
			} else {
				out[k] = "[REDACTED]"
			}
		default:
			out[k] = strings.Join(v, ",")
		}
	}
	return out
}

// Sign computes the signature header value for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "...[truncated]"
	}
	return string(b)
}

// ServeData handles /ns/{ns}/{mock}/{rest...}: the side the SUT calls.
func (h *Hub) ServeData(w http.ResponseWriter, r *http.Request, ns, mockName, rest string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	_ = r.Body.Close()
	e := Entry{Mock: mockName, Direction: "in", Method: r.Method, Path: rest, Query: r.URL.RawQuery, Body: truncate(body, 64<<10), Rule: -1}

	h.mu.Lock()
	n := h.nsLocked(ns)
	m, ok := n.mocks[mockName]
	if !ok {
		h.mu.Unlock()
		e.Unconfigured = true
		e.Status = http.StatusNotFound
		e.Headers = redactHeaders(r.Header, nil, "")
		h.Record(ns, e)
		http.Error(w, fmt.Sprintf("mockhub: mock %q not configured in namespace %q", mockName, ns), http.StatusNotFound)
		return
	}
	sigHeader := ""
	if m.cfg.Signature != nil {
		sigHeader = m.cfg.Signature.Header
	}
	e.Headers = redactHeaders(r.Header, m.cfg.Redact, sigHeader)

	// 1. schema validation against the real API's OpenAPI document
	if m.spec != nil {
		vr := r.Clone(r.Context())
		vr.URL.Path = rest
		vr.URL.RawPath = ""
		vr.RequestURI = ""
		vr.Body = io.NopCloser(bytes.NewReader(body))
		route, params, err := m.spec.router.FindRoute(vr)
		if err != nil {
			e.SchemaErrors = append(e.SchemaErrors, "no operation for "+r.Method+" "+rest+": "+err.Error())
		} else {
			e.Operation = route.Operation.OperationID
			in := &openapi3filter.RequestValidationInput{Request: vr, PathParams: params, Route: route,
				Options: &openapi3filter.Options{MultiError: true, AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
			if err := openapi3filter.ValidateRequest(r.Context(), in); err != nil {
				e.SchemaErrors = append(e.SchemaErrors, splitMulti(err)...)
			}
		}
	}
	// 2. signature
	if sc := m.cfg.Signature; sc != nil {
		valid := hmac.Equal([]byte(r.Header.Get(sc.Header)), []byte(Sign(sc.Secret, body)))
		e.SignatureValid = &valid
	}
	// 3. idempotent replay
	if hk := m.cfg.IdempotencyHeader; hk != "" {
		e.IdempotencyKey = r.Header.Get(hk)
		if st, ok := m.idem[e.IdempotencyKey]; ok && e.IdempotencyKey != "" {
			h.mu.Unlock()
			e.Replayed = true
			e.Status = st.status
			e.RespBody = truncate(st.body, 16<<10)
			h.Record(ns, e)
			writeResp(w, st.status, st.headers, st.body)
			return
		}
	}
	// 4. scripted response, else the OpenAPI example, else 200 {}
	resp, ruleIdx := m.pick(r.Method, rest, e.Operation)
	e.Rule = ruleIdx
	h.mu.Unlock()
	if resp == nil {
		resp = m.defaultResponse(e.Operation, 0)
	} else if len(resp.Body) == 0 && resp.BodyText == "" && resp.Fault == "" {
		// Status-only script entries answer with the documented example of that status.
		if ex := m.defaultResponse(e.Operation, resp.Status); ex.Status == resp.Status && len(ex.Body) > 0 && string(ex.Body) != "{}" {
			cp := *resp
			cp.Body = ex.Body
			resp = &cp
		}
	}

	delay := h.delay(resp.Delay)
	e.DelayMS = float64(delay) / float64(time.Millisecond)
	e.Fault = resp.Fault
	// Journal the call as soon as it arrives: crash tests need to know that a
	// request reached the provider even if the answer never makes it back.
	e.InFlight = true
	seq := h.Record(ns, e)
	finish := func(fn func(*Entry)) { h.update(ns, seq, fn) }
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			finish(func(x *Entry) { x.Error = "client gave up during delay (connection closed)" })
			return
		}
	}
	respBody := render(resp, body, ns)
	headers := map[string]string{}
	for k, v := range resp.Headers {
		headers[k] = v
	}
	if resp.RetryAfter != "" {
		headers["Retry-After"] = resp.RetryAfter
	}
	if _, ok := headers["Content-Type"]; !ok && len(respBody) > 0 && resp.BodyText == "" {
		headers["Content-Type"] = "application/json"
	}

	switch resp.Fault {
	case FaultReset:
		finish(func(x *Entry) { x.Status = 0 })
		hijackReset(w)
		return
	case FaultHang:
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Minute):
		}
		finish(func(x *Entry) { x.Status = 0; x.Error = "hung until the client gave up" })
		return
	case FaultClose:
		finish(func(x *Entry) { x.Status = resp.Status; x.RespBody = truncate(respBody, 16<<10) })
		hijackTruncate(w, resp.Status, headers, respBody)
		return
	}
	if hk := m.cfg.IdempotencyHeader; hk != "" && e.IdempotencyKey != "" && resp.Status >= 200 && resp.Status < 300 {
		h.mu.Lock()
		m.idem[e.IdempotencyKey] = storedResp{status: resp.Status, headers: headers, body: respBody}
		h.mu.Unlock()
	}
	writeResp(w, resp.Status, headers, respBody)
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	finish(func(x *Entry) { x.Status = status; x.RespBody = truncate(respBody, 16<<10) })
}

func writeResp(w http.ResponseWriter, status int, headers map[string]string, body []byte) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func hijackReset(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) // close with RST
	}
	_ = conn.Close()
}

func hijackTruncate(w http.ResponseWriter, status int, headers map[string]string, body []byte) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	if status == 0 {
		status = 200
	}
	fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	for k, v := range headers {
		fmt.Fprintf(buf, "%s: %s\r\n", k, v)
	}
	fmt.Fprintf(buf, "Content-Length: %d\r\n\r\n", len(body)+100)
	_, _ = buf.Write(body[:len(body)/2])
	_ = buf.Flush()
	_ = conn.Close()
}

func splitMulti(err error) []string {
	if me, ok := err.(openapi3.MultiError); ok {
		var out []string
		for _, e := range me {
			out = append(out, oneLine(e.Error()))
		}
		return out
	}
	return []string{oneLine(err.Error())}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

func (m *mock) pick(method, p, op string) (*Response, int) {
	for i, r := range m.rules {
		if r.Match.Method != "" && !strings.EqualFold(r.Match.Method, method) {
			continue
		}
		if r.Match.Operation != "" && r.Match.Operation != op {
			continue
		}
		if r.Match.Path != "" && r.Match.Path != p {
			if ok, _ := path.Match(r.Match.Path, p); !ok {
				continue
			}
		}
		idx := r.next
		if idx >= len(r.Responses) {
			idx = len(r.Responses) - 1
		} else {
			r.next++
		}
		resp := r.Responses[idx]
		return &resp, i
	}
	return nil, -1
}

// defaultResponse answers with the example of the operation for status
// (0: the first documented 2xx).
func (m *mock) defaultResponse(op string, status int) *Response {
	if m.spec != nil && op != "" {
		for _, pi := range m.spec.doc.Paths.Map() {
			for _, o := range pi.Operations() {
				if o.OperationID != op || o.Responses == nil {
					continue
				}
				codes := make([]string, 0)
				for code := range o.Responses.Map() {
					codes = append(codes, code)
				}
				sort.Strings(codes)
				for _, code := range codes {
					if status == 0 && !strings.HasPrefix(code, "2") || status != 0 && code != fmt.Sprint(status) {
						continue
					}
					status := 200
					fmt.Sscanf(code, "%d", &status)
					ref := o.Responses.Map()[code]
					if ref.Value == nil {
						continue
					}
					if mt := ref.Value.Content.Get("application/json"); mt != nil {
						var ex any = mt.Example
						if ex == nil {
							for _, e := range mt.Examples {
								if e.Value != nil {
									ex = e.Value.Value
									break
								}
							}
						}
						if ex != nil {
							b, _ := json.Marshal(ex)
							return &Response{Status: status, Body: b}
						}
					}
					return &Response{Status: status, Body: json.RawMessage(`{}`)}
				}
			}
		}
	}
	return &Response{Status: 200, Body: json.RawMessage(`{}`)}
}

func (h *Hub) delay(d *Delay) time.Duration {
	if d == nil {
		return 0
	}
	if d.Fixed > 0 {
		return time.Duration(d.Fixed)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var v float64
	switch d.Dist {
	case "uniform":
		v = float64(d.Min) + h.rnd.Float64()*float64(d.Max-d.Min)
	case "normal":
		v = h.rnd.NormFloat64()*float64(d.StdDev) + float64(d.Mean)
	case "exponential":
		v = h.rnd.ExpFloat64() * float64(d.Mean)
	default:
		return 0
	}
	if d.Min > 0 {
		v = math.Max(v, float64(d.Min))
	}
	if d.Max > 0 {
		v = math.Min(v, float64(d.Max))
	}
	return time.Duration(math.Max(v, 0))
}

var placeholderRe = regexp.MustCompile(`\$\{(req\.body\.[A-Za-z0-9_.]+|seq|ns|uuid|now)\}`)

// render resolves ${req.body.x}, ${uuid}, ${now}, ${ns} placeholders.
func render(resp *Response, reqBody []byte, ns string) []byte {
	raw := []byte(resp.BodyText)
	if resp.BodyText == "" {
		raw = resp.Body
	}
	if !bytes.Contains(raw, []byte("${")) {
		return raw
	}
	var req map[string]any
	_ = json.Unmarshal(reqBody, &req)
	return placeholderRe.ReplaceAllFunc(raw, func(m []byte) []byte {
		key := string(m[2 : len(m)-1])
		switch key {
		case "ns":
			return []byte(ns)
		case "now":
			return []byte(time.Now().UTC().Format(time.RFC3339Nano))
		case "uuid":
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			return []byte(hex.EncodeToString(b))
		}
		var cur any = req
		for _, part := range strings.Split(strings.TrimPrefix(key, "req.body."), ".") {
			mm, ok := cur.(map[string]any)
			if !ok {
				return []byte("")
			}
			cur = mm[part]
		}
		switch v := cur.(type) {
		case string:
			return []byte(v)
		case nil:
			return []byte("")
		default:
			b, _ := json.Marshal(v)
			return b
		}
	})
}

// SendWebhook calls the service under test (reverse direction) and journals it.
func (h *Hub) SendWebhook(ctx context.Context, ns string, req WebhookRequest) ([]WebhookResult, error) {
	if req.URL == "" {
		return nil, fmt.Errorf("url is required")
	}
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	header := req.Header
	if header == "" {
		header = "X-Signature"
	}
	sign := req.Sign
	if sign == "" && req.Secret != "" {
		sign = "valid"
	}
	n := req.Repeat
	if n <= 0 {
		n = 1
	}
	if req.Delay > 0 {
		select {
		case <-time.After(time.Duration(req.Delay)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var out []WebhookResult
	for i := 0; i < n; i++ {
		hr, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Content-Type", "application/json")
		for k, v := range req.Headers {
			hr.Header.Set(k, v)
		}
		switch sign {
		case "valid":
			hr.Header.Set(header, Sign(req.Secret, req.Body))
		case "invalid":
			hr.Header.Set(header, Sign(req.Secret+"-wrong", req.Body))
		}
		e := Entry{Mock: req.Mock, Direction: "out", Method: method, Path: req.URL, Body: truncate(req.Body, 64<<10),
			Headers: redactHeaders(hr.Header, nil, header), Rule: -1}
		res := WebhookResult{}
		resp, err := h.client.Do(hr)
		if err != nil {
			res.Error = err.Error()
			e.Error = err.Error()
		} else {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			res.Status, res.Body = resp.StatusCode, string(b)
			e.Status, e.RespBody = resp.StatusCode, string(b)
		}
		res.Seq = h.Record(ns, e)
		out = append(out, res)
	}
	return out, nil
}
