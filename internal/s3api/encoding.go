package s3api

import (
	"net/http"
	"sort"
	"strings"
)

// uriEncode percent-encodes per the SigV4 rules, which are stricter than Go's
// url package: everything outside the unreserved set is encoded, hex digits are
// uppercase, and a space is %20 rather than +.
//
// Go's url.QueryEscape and url.PathEscape both leave characters unescaped that
// AWS escapes, so neither can be substituted here.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isUnreserved(c):
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

const upperHex = "0123456789ABCDEF"

// isUnreserved matches RFC 3986's unreserved set, which is exactly what SigV4
// leaves untouched.
func isUnreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' ||
		c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

// canonicalQuery builds the canonical query string: every parameter encoded,
// then sorted by encoded name and value, joined with &. Parameters without a
// value still get a trailing "=".
//
// The raw query is parsed by hand rather than with url.ParseQuery because that
// discards the distinction between "?acl" and "?acl=", which S3 uses to select
// between subresources, and because it accepts ";" as a separator.
//
// exclude names a parameter to drop, which presigned requests use to remove
// X-Amz-Signature from the string it is signing.
func canonicalQuery(rawQuery, exclude string) string {
	if rawQuery == "" {
		return ""
	}

	type param struct{ name, value string }
	var params []param

	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		// Decode first, then re-encode: the client signed the canonical
		// encoding, which may differ from whatever it actually transmitted.
		decodedName, err := percentDecode(name)
		if err != nil {
			decodedName = name
		}
		if exclude != "" && strings.EqualFold(decodedName, exclude) {
			continue
		}
		decodedValue, err := percentDecode(value)
		if err != nil {
			decodedValue = value
		}
		params = append(params, param{
			name:  uriEncode(decodedName, true),
			value: uriEncode(decodedValue, true),
		})
	}

	sort.Slice(params, func(i, j int) bool {
		if params[i].name != params[j].name {
			return params[i].name < params[j].name
		}
		return params[i].value < params[j].value
	})

	var b strings.Builder
	for i, p := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.name)
		b.WriteByte('=')
		b.WriteString(p.value)
	}
	return b.String()
}

// percentDecode decodes %XX sequences. Unlike url.QueryUnescape it leaves "+"
// alone, because in a URI path and in SigV4's view of a query, "+" is a literal
// plus and not a space.
func percentDecode(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", errInvalidEscape
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return "", errInvalidEscape
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

var errInvalidEscape = &escapeError{}

type escapeError struct{}

func (*escapeError) Error() string { return "invalid percent-encoding" }

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// canonicalURIs returns the candidate canonical paths to try, most likely
// first.
//
// S3 signs the path as transmitted and explicitly does not normalize it, so the
// safest candidate is whatever the client actually put on the wire —
// URL.EscapedPath returns exactly that whenever the encoding is well-formed.
//
// The second candidate re-encodes the decoded path with AWS's rules. It covers
// the case where Go had to synthesise an encoding, since Go leaves sub-delims
// such as '$', '+' and ',' unescaped where AWS escapes them. Trying both costs
// one extra HMAC on the rare requests where the first candidate misses, and
// avoids rejecting keys that are otherwise perfectly legal.
func canonicalURIs(r *http.Request) []string {
	if r.URL.Path == "" {
		return []string{"/"}
	}

	asTransmitted := r.URL.EscapedPath()
	reEncoded := uriEncode(r.URL.Path, false)

	if asTransmitted == reEncoded {
		return []string{asTransmitted}
	}
	return []string{asTransmitted, reEncoded}
}
