// Package robots parses robots.txt (RFC 9309, plus Crawl-delay).
package robots

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// RFC 9309 requires at least 500 KiB
const maxSize = 512 << 10

// Rules is the policy that applies to one user agent.
type Rules struct {
	rules      []rule
	crawlDelay time.Duration
}

type rule struct {
	pattern string
	allow   bool
}

// AllowAll is the policy used when a site has no robots.txt.
var AllowAll = &Rules{}

// DisallowAll is the policy used when robots.txt returns a server error.
var DisallowAll = &Rules{rules: []rule{{pattern: "/", allow: false}}}

// Parse returns the rules for userAgent. Malformed lines are ignored.
func Parse(r io.Reader, userAgent string) *Rules {
	product := productToken(userAgent)

	type group struct {
		agents []string
		rules  []rule
		delay  time.Duration
	}
	var (
		groups   []*group
		current  *group
		inAgents bool // consecutive user-agent lines share a group
	)

	sc := bufio.NewScanner(io.LimitReader(r, maxSize))
	sc.Buffer(make([]byte, 0, 4096), maxSize)
	for sc.Scan() {
		key, val, ok := splitLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "user-agent":
			if !inAgents || current == nil {
				current = &group{}
				groups = append(groups, current)
			}
			current.agents = append(current.agents, strings.ToLower(val))
			inAgents = true
		case "allow", "disallow":
			inAgents = false
			if current == nil {
				continue
			}
			if key == "disallow" && val == "" {
				continue
			}
			current.rules = append(current.rules, rule{pattern: val, allow: key == "allow"})
		case "crawl-delay":
			inAgents = false
			if current == nil {
				continue
			}
			if secs, err := strconv.ParseFloat(val, 64); err == nil && secs > 0 && secs < 3600 {
				current.delay = time.Duration(secs * float64(time.Second))
			}
		default:
			inAgents = false
		}
	}

	var (
		best      *Rules
		bestLen   = -1
		wildcards *Rules
	)
	for _, g := range groups {
		for _, a := range g.agents {
			switch {
			case a == "*":
				if wildcards == nil {
					wildcards = &Rules{}
				}
				wildcards.rules = append(wildcards.rules, g.rules...)
				if g.delay > wildcards.crawlDelay {
					wildcards.crawlDelay = g.delay
				}
			case product != "" && strings.HasPrefix(product, a) && len(a) >= bestLen:
				if len(a) > bestLen {
					best = &Rules{}
					bestLen = len(a)
				}
				best.rules = append(best.rules, g.rules...)
				if g.delay > best.crawlDelay {
					best.crawlDelay = g.delay
				}
			}
		}
	}
	switch {
	case best != nil:
		return best
	case wildcards != nil:
		return wildcards
	default:
		return AllowAll
	}
}

// Allowed reports whether path (with query) may be fetched.
func (r *Rules) Allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	if path == "/robots.txt" {
		return true
	}
	allowed, bestLen := true, -1
	for _, ru := range r.rules {
		if !match(ru.pattern, path) {
			continue
		}
		n := len(ru.pattern)
		if n > bestLen || (n == bestLen && ru.allow) {
			allowed, bestLen = ru.allow, n
		}
	}
	return allowed
}

// CrawlDelay returns the Crawl-delay requested for this agent, or zero.
func (r *Rules) CrawlDelay() time.Duration { return r.crawlDelay }

func match(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	rest := path[len(parts[0]):]
	if len(parts) == 1 {
		return !anchored || rest == ""
	}
	for i, p := range parts[1:] {
		last := i == len(parts)-2
		if last && anchored {
			return strings.HasSuffix(rest, p)
		}
		idx := strings.Index(rest, p)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(p):]
	}
	return true
}

func splitLine(line string) (key, val string, ok bool) {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	k, v, found := strings.Cut(line, ":")
	if !found {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(k))
	return key, strings.TrimSpace(v), key != ""
}

func productToken(ua string) string {
	ua = strings.TrimSpace(ua)
	if i := strings.IndexAny(ua, "/ "); i >= 0 {
		ua = ua[:i]
	}
	return strings.ToLower(ua)
}
