package httpmock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client drives the hub control plane from the runner.
type Client struct {
	Base string // e.g. http://127.0.0.1:58081
	HTTP *http.Client
}

func NewClient(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

// DataURL is the base URL the service under test must call for a mock.
func DataURL(hubBase, ns, mock string) string {
	return fmt.Sprintf("%s/ns/%s/%s", hubBase, ns, mock)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mockhub %s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

func (c *Client) Configure(ctx context.Context, ns, mock string, cfg MockConfig) error {
	return c.do(ctx, http.MethodPut, "/_mock/ns/"+ns+"/mocks/"+mock, cfg, nil)
}

func (c *Client) Script(ctx context.Context, ns, mock string, s Script) error {
	return c.do(ctx, http.MethodPut, "/_mock/ns/"+ns+"/mocks/"+mock+"/script", s, nil)
}

func (c *Client) Journal(ctx context.Context, ns, mock string) ([]Entry, error) {
	var out []Entry
	p := "/_mock/ns/" + ns + "/journal"
	if mock != "" {
		p += "?mock=" + mock
	}
	return out, c.do(ctx, http.MethodGet, p, nil, &out)
}

func (c *Client) Mocks(ctx context.Context, ns string) ([]MockInfo, error) {
	var out []MockInfo
	return out, c.do(ctx, http.MethodGet, "/_mock/ns/"+ns+"/mocks", nil, &out)
}

func (c *Client) SendWebhook(ctx context.Context, ns string, req WebhookRequest) ([]WebhookResult, error) {
	var out []WebhookResult
	return out, c.do(ctx, http.MethodPost, "/_mock/ns/"+ns+"/webhooks", req, &out)
}

func (c *Client) Reset(ctx context.Context, ns string) error {
	return c.do(ctx, http.MethodDelete, "/_mock/ns/"+ns, nil, nil)
}

// ScriptSMTP sets the SMTP behaviours of a namespace (one per session).
func (c *Client) ScriptSMTP(ctx context.Context, ns string, behaviours []string) error {
	return c.do(ctx, http.MethodPut, "/_mock/ns/"+ns+"/smtp/script", map[string]any{"behaviours": behaviours}, nil)
}

// ConfigureSocket configures a WebSocket/TCP partner; TCP mocks get a port.
func (c *Client) ConfigureSocket(ctx context.Context, ns, mock string, cfg any) (int, error) {
	var out struct {
		Port int `json:"port"`
	}
	err := c.do(ctx, http.MethodPut, "/_mock/ns/"+ns+"/sockets/"+mock, cfg, &out)
	return out.Port, err
}
