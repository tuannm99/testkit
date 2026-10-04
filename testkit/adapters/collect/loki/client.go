// Package loki exports the logs of a run (selected by the run_id label) as
// JSON lines, one per log line, ordered by time.
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Line is one exported log line.
type Line struct {
	Time   time.Time         `json:"time"`
	Labels map[string]string `json:"labels"`
	Line   string            `json:"line"`
}

// QueryRange returns up to limit log lines matching a LogQL selector.
func (c *Client) QueryRange(ctx context.Context, logql string, start, end time.Time, limit int) ([]Line, error) {
	var out []Line
	cursor := start
	for len(out) < limit {
		q := url.Values{}
		q.Set("query", logql)
		q.Set("start", strconv.FormatInt(cursor.UnixNano(), 10))
		q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
		q.Set("limit", strconv.Itoa(min(5000, limit-len(out))))
		q.Set("direction", "forward")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/loki/api/v1/query_range?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("loki: HTTP %d: %.300s", resp.StatusCode, raw)
		}
		var r struct {
			Data struct {
				Result []struct {
					Stream map[string]string `json:"stream"`
					Values [][2]string       `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		var page []Line
		for _, s := range r.Data.Result {
			for _, v := range s.Values {
				ns, _ := strconv.ParseInt(v[0], 10, 64)
				page = append(page, Line{Time: time.Unix(0, ns).UTC(), Labels: s.Stream, Line: v[1]})
			}
		}
		if len(page) == 0 {
			break
		}
		sort.SliceStable(page, func(i, j int) bool { return page[i].Time.Before(page[j].Time) })
		out = append(out, page...)
		next := page[len(page)-1].Time.Add(time.Nanosecond)
		if !next.After(cursor) || len(page) < 5000 {
			break
		}
		cursor = next
	}
	return out, nil
}

// RangeRaw runs a LogQL range query (log or metric query) and returns the raw
// API response and its result type ("streams" or "matrix").
func (c *Client) RangeRaw(ctx context.Context, logql string, start, end time.Time, step time.Duration, limit int) ([]byte, string, error) {
	q := url.Values{}
	q.Set("query", logql)
	q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', 3, 64))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("direction", "forward")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/loki/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return raw, "", fmt.Errorf("loki: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	var r struct {
		Data struct {
			ResultType string `json:"resultType"`
		} `json:"data"`
	}
	err = json.Unmarshal(raw, &r)
	return raw, r.Data.ResultType, err
}
