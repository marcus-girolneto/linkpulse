// Package robots implements the subset of the Robots Exclusion Protocol
// (RFC 9309) a link checker needs: user-agent group selection, Allow and
// Disallow rules with "*" and "$" wildcards, longest-match precedence and the
// non-standard but widely used Crawl-delay directive.
package robots

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// maxSize caps how much of a robots.txt file is read. RFC 9309 requires
// crawlers to parse at least 500 KiB.
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

// DisallowAll is the policy used when robots.txt is unreachable because of a
// server error, as RFC 9309 recommends for unreachable files.
var DisallowAll = &Rules{rules: []rule{{pattern: "/", allow: false}}}

// Parse reads a robots.txt body and returns the rules for userAgent. The
// group whose user-agent token is the longest case-insensitive prefix of
// userAgent's product token wins; otherwise the "*" group applies.
// Malformed lines are ignored, so Parse never fails.
func Parse(r io.Reader, userAgent string) *Rules {
	product := productToken(userAgent)

	type group struct {
		agents []string
		rules  []rule
		delay  time.Duration
	}
	var (
		groups  []*group
		current *group
		// A run of consecutive user-agent lines shares the rules that follow.
		inAgents bool
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
				// "Disallow:" with no path allows everything.
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

// Allowed reports whether the path (including any query string) may be
// fetched. The most specific (longest) matching rule wins; on a tie Allow
// wins. "/robots.txt" is always allowed.
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

// match implements RFC 9309 path matching: "*" matches any sequence of
// characters and a trailing "$" anchors the pattern at the end of the path.
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

// productToken extracts the product name robots.txt groups match against,
// for example "linkpulse" from "linkpulse/1.2 (+https://...)".
func productToken(ua string) string {
	ua = strings.TrimSpace(ua)
	if i := strings.IndexAny(ua, "/ "); i >= 0 {
		ua = ua[:i]
	}
	return strings.ToLower(ua)
}
