// Package urlnorm canonicalizes URLs so that links which point at the same
// resource compare equal. It is deliberately conservative: it only applies
// transformations that are safe for every HTTP server (RFC 3986 section 6.2.2
// syntax-based normalization plus fragment removal) and never touches the
// query string or path case.
package urlnorm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrUnsupportedScheme is returned for references that are not http or https,
// such as mailto:, tel:, javascript: or data: URLs.
var ErrUnsupportedScheme = errors.New("unsupported scheme")

// ErrNoHost is returned when a reference resolves to a URL without a host.
var ErrNoHost = errors.New("missing host")

// Parse parses and normalizes an absolute http(s) URL.
func Parse(raw string) (*url.URL, error) {
	return Resolve(nil, raw)
}

// Resolve resolves ref against base (which may be nil for absolute refs) and
// returns the normalized result. The returned URL never has a fragment, its
// scheme and host are lowercase, default ports are removed and an empty path
// becomes "/".
func Resolve(base *url.URL, ref string) (*url.URL, error) {
	ref = clean(ref)
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", ref, err)
	}
	if base != nil {
		u = base.ResolveReference(u)
	} else if u.IsAbs() {
		// ResolveReference also removes dot segments, which we want for
		// absolute references too.
		u = u.ResolveReference(&url.URL{})
	}
	return normalize(u)
}

// String is a convenience wrapper around Resolve that returns the normalized
// URL in string form.
func String(base *url.URL, ref string) (string, error) {
	u, err := Resolve(base, ref)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func normalize(u *url.URL) (*url.URL, error) {
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		if u.Scheme == "" {
			return nil, fmt.Errorf("%w: relative reference without base", ErrNoHost)
		}
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedScheme, u.Scheme)
	}
	if u.Opaque != "" {
		return nil, fmt.Errorf("%w: opaque URL", ErrNoHost)
	}

	host := strings.ToLower(u.Hostname())
	host = strings.TrimRight(host, ".")
	if host == "" {
		return nil, ErrNoHost
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}

	u.Fragment = ""
	u.RawFragment = ""
	if u.Path == "" {
		u.Path = "/"
		u.RawPath = ""
	}
	// An empty "?" carries no information.
	u.ForceQuery = false
	// net/url keeps raw spaces in the query; browsers send them as %20.
	u.RawQuery = strings.ReplaceAll(u.RawQuery, " ", "%20")

	// Some inputs survive url.Parse only thanks to lenient handling of the
	// original form (for example odd IPv6 literals) but produce a string that
	// does not parse again. Rejecting them keeps normalization idempotent.
	if _, err := url.Parse(u.String()); err != nil {
		return nil, fmt.Errorf("normalized form is not a valid URL: %w", err)
	}
	return u, nil
}

// clean mimics how browsers pre-process href attributes: leading and trailing
// whitespace is trimmed and ASCII tab and newline characters are removed.
func clean(ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.ContainsAny(ref, "\t\n\r") {
		ref = strings.Map(func(r rune) rune {
			if r == '\t' || r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, ref)
	}
	return ref
}
