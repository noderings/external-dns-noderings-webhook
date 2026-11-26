package nrdns

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// NormalizeName lower-cases a DNS name and removes the trailing dot.
func NormalizeName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// RelativeName turns a name inside zone ("www.example.com", "www.example.com.",
// "example.com") into the API's relative form ("www", "@").
func RelativeName(fqdn, zone string) string {
	n, z := NormalizeName(fqdn), NormalizeName(zone)
	if n == "" || n == "@" || n == z {
		return "@"
	}
	return strings.TrimSuffix(n, "."+z)
}

// AbsoluteName turns an API name ("www", "@", or an absolute "www.example.com.")
// into a full name without the trailing dot.
func AbsoluteName(name, zone string) string {
	n, z := strings.ToLower(strings.TrimSpace(name)), NormalizeName(zone)
	switch {
	case n == "" || n == "@":
		return z
	case strings.HasSuffix(n, "."):
		return strings.TrimSuffix(n, ".")
	case n == z || strings.HasSuffix(n, "."+z):
		return n
	}
	return n + "." + z
}

// InZone reports whether name is zone or a name below it.
func InZone(name, zone string) bool {
	n, z := NormalizeName(name), NormalizeName(zone)
	return n == z || strings.HasSuffix(n, "."+z)
}

// FindZone returns the zone with the longest name that contains name.
func FindZone(zones []Zone, name string) (Zone, bool) {
	var best Zone
	found := false
	for _, z := range zones {
		if InZone(name, z.Name) && (!found || len(NormalizeName(z.Name)) > len(NormalizeName(best.Name))) {
			best, found = z, true
		}
	}
	return best, found
}

// QuoteTXT turns plain text into the presentation format the API stores:
// one or more quoted character-strings of at most 255 bytes. A value that is
// already quoted is returned unchanged.
func QuoteTXT(v string) string {
	if t := strings.TrimSpace(v); len(t) >= 2 && strings.HasPrefix(t, `"`) && strings.HasSuffix(t, `"`) {
		return t
	}
	var parts []string
	for len(v) > 255 {
		cut := 255
		for cut > 0 && v[cut]&0xC0 == 0x80 {
			cut--
		}
		parts = append(parts, v[:cut])
		v = v[cut:]
	}
	parts = append(parts, v)
	for i, p := range parts {
		p = strings.ReplaceAll(p, `\`, `\\`)
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
	}
	return strings.Join(parts, " ")
}

// UnquoteTXT joins the character-strings of a presentation-format TXT value.
// A value without quotes is returned unchanged.
func UnquoteTXT(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, `"`) {
		return v
	}
	var b strings.Builder
	in := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			in = !in
		case c == '\\' && in && i+1 < len(v):
			i++
			b.WriteByte(v[i])
		case in:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ValidateValues checks values for a record type before they reach the API.
func ValidateValues(rrtype string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s: at least one value is required", rrtype)
	}
	if (rrtype == "CNAME" || rrtype == "ALIAS" || rrtype == "DNAME") && len(values) > 1 {
		return fmt.Errorf("%s records take exactly one value, got %d", rrtype, len(values))
	}
	for _, v := range values {
		switch rrtype {
		case "A":
			if ip, err := netip.ParseAddr(v); err != nil || !ip.Is4() {
				return fmt.Errorf("%q is not an IPv4 address", v)
			}
		case "AAAA":
			if ip, err := netip.ParseAddr(v); err != nil || !ip.Is6() || ip.Is4In6() {
				return fmt.Errorf("%q is not an IPv6 address", v)
			}
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s: values must not be empty", rrtype)
		}
	}
	return nil
}

// SortedUnique returns values sorted with duplicates removed.
func SortedUnique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
