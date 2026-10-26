package argus

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Privacy defaults (spec §7). Argus scrubs again on arrival; the SDK doesn't
// rely on that.

// Filtered replaces every sensitive value.
const Filtered = "[Filtered]"

var sensitiveContains = []string{
	"password", "passwd", "secret", "token", "apikey", "accesskey", "authorization",
	"credential", "cookie", "privatekey", "cardnumber", "creditcard", "bvn", "cvv", "cvc",
	"sessionid", "sessiontoken", "signature",
}

var sensitiveExact = map[string]bool{
	"auth": true, "pwd": true, "pin": true, "otp": true, "session": true, "card": true, "sid": true,
}

var keyNormalizer = strings.NewReplacer("-", "", "_", "", " ", "")

// sensitiveKey reports whether a field's value must be filtered: lowercased
// with -, _ and spaces removed, it contains or equals one of the spec's words.
func sensitiveKey(key string) bool {
	n := keyNormalizer.Replace(strings.ToLower(key))
	if sensitiveExact[n] {
		return true
	}
	for _, w := range sensitiveContains {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

var (
	cardRe   = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`)
	jwtRe    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`)
	// Any key=value or key: value; the key is then checked with sensitiveKey,
	// so the string rule is exactly the field rule.
	// A quote after the key covers JSON text: "pin": "1234".
	kvRe = regexp.MustCompile(`\b([A-Za-z0-9_-]+)(["']?\s*[:=]\s*)(["']?)([^"'\s,;&})]+)`)
)

func luhn(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// scrubString filters card numbers, credentials, JWTs and sensitive
// key=value pairs inside a string.
func scrubString(s string) string {
	if s == "" {
		return s
	}
	if strings.ContainsAny(s, "0123456789") {
		s = cardRe.ReplaceAllStringFunc(s, func(m string) string {
			d := strings.NewReplacer(" ", "", "-", "").Replace(m)
			if len(d) >= 13 && len(d) <= 19 && luhn(d) {
				return Filtered
			}
			return m
		})
	}
	s = bearerRe.ReplaceAllString(s, "${1} "+Filtered)
	s = jwtRe.ReplaceAllString(s, Filtered)
	if strings.ContainsAny(s, ":=") {
		s = scrubPairs(s)
	}
	return s
}

// scrubPairs filters the value of each key=value (or key: value) pair whose
// key is sensitive. "Authorization: Bearer [Filtered]" is left readable.
func scrubPairs(s string) string {
	var b strings.Builder
	last, pos := 0, 0
	for pos < len(s) {
		m := kvRe.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		for i := range m {
			if m[i] >= 0 {
				m[i] += pos
			}
		}
		key, val := s[m[2]:m[3]], s[m[8]:m[9]]
		scheme := strings.ToLower(val)
		keep := !sensitiveKey(key) || val == Filtered ||
			((scheme == "bearer" || scheme == "basic" || scheme == "token") && strings.HasPrefix(s[m[9]:], " "+Filtered))
		if keep {
			// The value may itself hold a pair ("error: password=x"): look again
			// from the end of this key.
			pos = m[3]
			continue
		}
		b.WriteString(s[last:m[8]])
		b.WriteString(Filtered)
		last, pos = m[9], m[9]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// scrubValue deep-scrubs a JSON-shaped value (maps, slices, strings).
func scrubValue(v any, depth int) any {
	switch t := v.(type) {
	case string:
		return scrubString(t)
	case map[string]any:
		if depth > 12 {
			return "[…]"
		}
		for k, val := range t {
			if sensitiveKey(k) && val != nil && val != "" {
				t[k] = Filtered
			} else {
				t[k] = scrubValue(val, depth+1)
			}
		}
		return t
	case []any:
		if depth > 12 {
			return "[…]"
		}
		for i := range t {
			t[i] = scrubValue(t[i], depth+1)
		}
		return t
	default:
		return v
	}
}

// scrubEvent marshals an event, scrubs everything but the sdk block, and
// returns the JSON to send.
func scrubEvent(e *Event) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	sdk := m["sdk"]
	delete(m, "sdk")
	scrubValue(m, 0)
	m["sdk"] = sdk
	return json.Marshal(m)
}

// jsonSafe replaces values that can't be marshalled (channels, funcs,
// cycles) with their type name, so one bad value never loses the whole event.
func jsonSafe(m map[string]any) {
	for k, v := range m {
		if _, err := json.Marshal(v); err != nil {
			m[k] = fmt.Sprintf("[%T: not JSON]", v)
		}
	}
}

// scrubAttrs filters sensitive attribute keys and values, and strips query
// strings from URL attributes.
func scrubAttrs(attrs []attr) []attr {
	for i := range attrs {
		if sensitiveKey(attrs[i].key) {
			if s, ok := attrs[i].val.(string); !ok || s != "" {
				attrs[i].val = Filtered
			}
			continue
		}
		if s, ok := attrs[i].val.(string); ok {
			if attrs[i].key == "url.full" || attrs[i].key == "http.url" || attrs[i].key == "url" {
				s = stripQuery(s)
			}
			attrs[i].val = scrubString(s)
		}
	}
	return attrs
}

// stripQuery removes the query string, fragment and any user:password from a URL.
func stripQuery(raw string) string {
	if u, err := url.Parse(raw); err == nil && (u.Scheme != "" || u.Host != "") {
		u.User = nil
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		u.RawFragment = ""
		return u.String()
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}
	return raw
}
