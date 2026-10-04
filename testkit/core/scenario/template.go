package scenario

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

var wholeAction = regexp.MustCompile(`^\{\{\s*(\.[A-Za-z0-9_.]+)\s*\}\}$`)

// RenderString renders a Go template against data. A string that is exactly
// one field reference ("{{ .input.order }}") keeps the referenced value's
// type (map, list, number), so fixtures can be reused without quoting.
func RenderString(s string, data map[string]any) (any, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	if m := wholeAction.FindStringSubmatch(s); m != nil {
		v, ok := lookup(data, strings.Split(strings.TrimPrefix(m[1], "."), "."))
		if !ok {
			return nil, fmt.Errorf("template %q: %s is not defined", s, m[1])
		}
		if _, isStr := v.(string); !isStr {
			return v, nil
		}
	}
	t, err := template.New("v").Option("missingkey=error").Funcs(templateFuncs).Parse(s)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", s, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("template %q: %w", s, err)
	}
	return b.String(), nil
}

var templateFuncs = template.FuncMap{
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"json": func(v any) (string, error) {
		b, err := jsonMarshal(v)
		return string(b), err
	},
}

func lookup(data map[string]any, path []string) (any, bool) {
	var cur any = data
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Data builds the template data of one execution and renders vars and input
// (vars first; input may reference vars).
func (c *Case) Data(runID, ns, trigger string) (map[string]any, error) {
	data := map[string]any{"run_id": runID, "ns": ns, "trigger": trigger, "case": c.ID}
	vars := map[string]any{}
	data["vars"] = vars
	// vars may reference earlier vars: render in a stable order, retrying until fixed point.
	pending := map[string]any{}
	for k, v := range c.Vars {
		pending[k] = v
	}
	for len(pending) > 0 {
		progress := false
		var lastErr error
		for k, v := range pending {
			r, err := Render(v, data)
			if err != nil {
				lastErr = err
				continue
			}
			vars[k] = r
			delete(pending, k)
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("vars: %w", lastErr)
		}
	}
	in, err := Render(map[string]any(c.Input), data)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	if in == nil {
		in = map[string]any{}
	}
	data["input"] = in
	return data, nil
}
