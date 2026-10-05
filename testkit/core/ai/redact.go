// Package ai is the optional assistant layer: it drafts test cases, suggests
// a triage for failures and summarises runs. It never decides a result —
// results come from assertions, thresholds and the gate. Everything it sends
// is redacted first and checked again; anything still sensitive is not sent.
package ai

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/tuannm99/testkit/testkit/core/evidence"
)

// Redactor replaces sensitive values with stable placeholders: the same value
// always maps to the same placeholder within one request, so the model can
// still correlate ("<email-1> received two mails") without seeing the value.
// The mapping is kept in memory only.
type Redactor struct {
	secrets map[string]string
	seen    map[string]string
	next    map[string]int
	counts  map[string]int
}

func NewRedactor(secrets map[string]string) *Redactor {
	return &Redactor{secrets: secrets, seen: map[string]string{}, next: map[string]int{}, counts: map[string]int{}}
}

var (
	reURLCreds = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^\s:/@]+):([^\s@/]+)@`)
	reKV       = regexp.MustCompile(`(?i)("?\b(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|authorization|signature|x-signature|sig|cookie|set-cookie)\b"?\s*[:=]\s*"?)([^"\s,;&}]+)`)
	reAuth     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	reJWT      = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`)
	reEmail    = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)
	reDigits   = regexp.MustCompile(`\b\d{9,19}\b`)
	rePhone    = regexp.MustCompile(`\b(?:\+\d{1,3}[ .-]?)?\(?\d{2,4}\)?[ .-]\d{3,4}[ .-]\d{3,4}\b`)
	reIPv4     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reToken    = regexp.MustCompile(`[A-Za-z0-9+_=-]{32,}`)
	reHex      = regexp.MustCompile(`^[0-9a-fA-F]{32,}$`)
)

func (r *Redactor) placeholder(kind, value string) string {
	r.counts[kind]++
	if p, ok := r.seen[kind+"\x00"+value]; ok {
		return p
	}
	r.next[kind]++
	p := fmt.Sprintf("<%s-%d>", kind, r.next[kind])
	r.seen[kind+"\x00"+value] = p
	return p
}

// Text redacts one text.
func (r *Redactor) Text(s string) string {
	// Declared secrets first, longest first (one may contain another).
	names := make([]string, 0, len(r.secrets))
	for k, v := range r.secrets {
		if len(v) >= 4 {
			names = append(names, k)
		}
	}
	sort.Slice(names, func(i, j int) bool { return len(r.secrets[names[i]]) > len(r.secrets[names[j]]) })
	for _, k := range names {
		if strings.Contains(s, r.secrets[k]) {
			r.counts["secret"] += strings.Count(s, r.secrets[k])
			s = strings.ReplaceAll(s, r.secrets[k], "<secret:"+k+">")
		}
	}
	s = reURLCreds.ReplaceAllStringFunc(s, func(m string) string {
		g := reURLCreds.FindStringSubmatch(m)
		r.counts["credential"]++
		return g[1] + "<user>:<redacted>@"
	})
	s = reAuth.ReplaceAllStringFunc(s, func(m string) string {
		r.counts["credential"]++
		return strings.Fields(m)[0] + " <redacted>"
	})
	s = reKV.ReplaceAllStringFunc(s, func(m string) string {
		g := reKV.FindStringSubmatch(m)
		if strings.HasPrefix(g[2], "<") { // already a placeholder
			return m
		}
		r.counts["credential"]++
		return g[1] + "<redacted>"
	})
	s = reJWT.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder("token", m) })
	s = reEmail.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder("email", strings.ToLower(m)) })
	s = reDigits.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) >= 13 && luhn(m) {
			return r.placeholder("card", m)
		}
		return r.placeholder("number", m)
	})
	s = rePhone.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder("phone", m) })
	s = reIPv4.ReplaceAllStringFunc(s, func(m string) string {
		if m == "127.0.0.1" || m == "0.0.0.0" {
			return m
		}
		return r.placeholder("ip", m)
	})
	// Opaque tokens: long segments that look random (hex digests, keys,
	// session ids). Paths and readable identifiers are left alone.
	s = reToken.ReplaceAllStringFunc(s, func(m string) string {
		if reHex.MatchString(m) || entropy(m) >= 4.2 {
			return r.placeholder("token", m)
		}
		return m
	})
	return s
}

// Counts reports what was redacted, by kind (never the values).
func (r *Redactor) Counts() map[string]int {
	out := map[string]int{}
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

// entropy is the Shannon entropy in bits per character.
func entropy(s string) float64 {
	freq := map[rune]float64{}
	for _, c := range s {
		freq[c]++
	}
	n := float64(len([]rune(s)))
	e := 0.0
	for _, f := range freq {
		p := f / n
		e -= p * math.Log2(p)
	}
	return e
}

func luhn(s string) bool {
	sum, alt := 0, false
	for i := len(s) - 1; i >= 0; i-- {
		d := int(s[i] - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// Guard checks a redacted payload before it leaves: a declared secret, a
// credential pattern, an e-mail address or a card number still present means
// the redaction missed something, and nothing is sent.
func Guard(payload string, secrets map[string]string) error {
	var problems []string
	for _, f := range evidence.ScanText(payload, secrets) {
		problems = append(problems, fmt.Sprintf("line %d: %s", f.Line, f.What))
	}
	for n, line := range strings.Split(payload, "\n") {
		if reEmail.MatchString(line) {
			problems = append(problems, fmt.Sprintf("line %d: e-mail address", n+1))
		}
		for _, m := range reDigits.FindAllString(line, -1) {
			if len(m) >= 13 && luhn(m) {
				problems = append(problems, fmt.Sprintf("line %d: card number", n+1))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("refusing to send: the redacted payload still contains sensitive data (%s)", strings.Join(problems, "; "))
	}
	return nil
}
