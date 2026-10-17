// Package urlnorm canonicalizes http(s) URLs per RFC 3986 section 6.2.2.
package urlnorm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrUnsupportedScheme is returned for non-http(s) references.
var ErrUnsupportedScheme = errors.New("unsupported scheme")

// ErrNoHost is returned when a reference resolves to a URL without a host.
var ErrNoHost = errors.New("missing host")

// Parse parses and normalizes an absolute http(s) URL.
func Parse(raw string) (*url.URL, error) {
	return Resolve(nil, raw)
}

// Resolve resolves ref against base (nil for absolute refs) and normalizes it.
func Resolve(base *url.URL, ref string) (*url.URL, error) {
	ref = clean(ref)
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", ref, err)
	}
	if base != nil {
		u = base.ResolveReference(u)
	} else if u.IsAbs() {
		// removes dot segments
		u = u.ResolveReference(&url.URL{})
	}
	return normalize(u)
}

// String is like Resolve but returns a string.
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
	u.ForceQuery = false
	// browsers send query spaces as %20
	u.RawQuery = strings.ReplaceAll(u.RawQuery, " ", "%20")

	// some odd inputs (IPv6 literals) parse once but not twice
	if _, err := url.Parse(u.String()); err != nil {
		return nil, fmt.Errorf("normalized form is not a valid URL: %w", err)
	}
	return u, nil
}

// clean strips whitespace the way browsers do for href values.
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
