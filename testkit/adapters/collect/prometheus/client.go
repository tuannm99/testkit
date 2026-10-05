// Package prometheus fetches raw time series (query_range) as evidence. The
// raw API response is kept byte for byte; a CSV is derived from it.
package prometheus

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Series is one labelled series of a matrix result.
type Series struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"` // [unix seconds (float), "value"]
}

// Result is a parsed query_range answer plus the raw bytes.
type Result struct {
	Raw    []byte
	Series []Series
}

type apiResp struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, *apiResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	var ar apiResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		return raw, nil, fmt.Errorf("prometheus %s: HTTP %d: %.200s", path, resp.StatusCode, raw)
	}
	if ar.Status != "success" {
		return raw, &ar, fmt.Errorf("prometheus %s: %s: %s", path, ar.ErrorType, ar.Error)
	}
	return raw, &ar, nil
}

// QueryRange runs a range query.
func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (*Result, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64))
	q.Set("end", strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', 3, 64))
	raw, ar, err := c.get(ctx, "/api/v1/query_range", q)
	if err != nil {
		return &Result{Raw: raw}, err
	}
	res := &Result{Raw: raw}
	if err := json.Unmarshal(ar.Data.Result, &res.Series); err != nil {
		return res, err
	}
	return res, nil
}

// Scalar evaluates an instant query that must return exactly one sample.
func (c *Client) Scalar(ctx context.Context, query string, at time.Time) (float64, bool, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("time", strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64))
	_, ar, err := c.get(ctx, "/api/v1/query", q)
	if err != nil {
		return 0, false, err
	}
	var vec []struct {
		Value [2]any `json:"value"`
	}
	if ar.Data.ResultType == "scalar" {
		var v [2]any
		_ = json.Unmarshal(ar.Data.Result, &v)
		f, _ := strconv.ParseFloat(fmt.Sprint(v[1]), 64)
		return f, true, nil
	}
	if err := json.Unmarshal(ar.Data.Result, &vec); err != nil {
		return 0, false, err
	}
	if len(vec) == 0 {
		return 0, false, nil
	}
	if len(vec) > 1 {
		return 0, false, fmt.Errorf("query returned %d series, expected 1: %s", len(vec), query)
	}
	f, err := strconv.ParseFloat(fmt.Sprint(vec[0].Value[1]), 64)
	return f, err == nil, err
}

// LabelString renders a series identity as {a="b", c="d"}.
func LabelString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// WriteCSV writes one row per sample: unix_ts, iso_time, series, value.
func (r *Result) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"unix_ts", "time_utc", "series", "value"})
	for _, s := range r.Series {
		id := LabelString(s.Metric)
		for _, v := range s.Values {
			ts, _ := v[0].(float64)
			sec, frac := int64(ts), ts-float64(int64(ts))
			t := time.Unix(sec, int64(frac*1e9)).UTC()
			_ = cw.Write([]string{strconv.FormatFloat(ts, 'f', 3, 64), t.Format(time.RFC3339Nano), id, fmt.Sprint(v[1])})
		}
	}
	cw.Flush()
	return cw.Error()
}

// Stats summarises every series (min/max/last) for the report.
type Stats struct {
	Series string  `json:"series"`
	Points int     `json:"points"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Last   float64 `json:"last"`
}

func (r *Result) Stats() []Stats {
	var out []Stats
	for _, s := range r.Series {
		st := Stats{Series: LabelString(s.Metric)}
		for i, v := range s.Values {
			f, err := strconv.ParseFloat(fmt.Sprint(v[1]), 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				continue // NaN/Inf (e.g. a quantile of no data) is kept in the raw file only
			}
			if st.Points == 0 || f < st.Min {
				st.Min = f
			}
			if st.Points == 0 || f > st.Max {
				st.Max = f
			}
			st.Last = f
			st.Points++
			_ = i
		}
		out = append(out, st)
	}
	return out
}
