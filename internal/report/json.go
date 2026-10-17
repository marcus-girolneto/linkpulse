package report

import (
	"encoding/json"
	"io"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/check"
	"github.com/marcus-girolneto/linkpulse/internal/crawl"
)

// The JSON document is a public contract consumed by scripts and CI jobs, so
// it has its own types rather than exposing crawl's internals directly.
type jsonReport struct {
	Start       string     `json:"start"`
	StartedAt   time.Time  `json:"started_at"`
	DurationMS  int64      `json:"duration_ms"`
	Interrupted bool       `json:"interrupted"`
	Summary     jsonStats  `json:"summary"`
	Links       []jsonLink `json:"links"`
}

type jsonStats struct {
	Pages      int `json:"pages"`
	Total      int `json:"total"`
	OK         int `json:"ok"`
	Broken     int `json:"broken"`
	Redirected int `json:"redirected"`
	Skipped    int `json:"skipped"`
	Unchecked  int `json:"unchecked"`
}

type jsonLink struct {
	URL          string       `json:"url"`
	State        string       `json:"state"`
	Status       int          `json:"status,omitempty"`
	Error        string       `json:"error,omitempty"`
	SkipReason   string       `json:"skip_reason,omitempty"`
	Internal     bool         `json:"internal"`
	Page         bool         `json:"page"`
	Depth        int          `json:"depth"`
	Method       string       `json:"method,omitempty"`
	Redirects    []check.Hop  `json:"redirects,omitempty"`
	FinalURL     string       `json:"final_url,omitempty"`
	Attempts     int          `json:"attempts,omitempty"`
	DurationMS   int64        `json:"duration_ms,omitempty"`
	FoundOn      []jsonSource `json:"found_on,omitempty"`
	FoundOnCount int          `json:"found_on_count"`
}

type jsonSource struct {
	Page string `json:"page"`
	Tag  string `json:"tag"`
	Attr string `json:"attr"`
}

// Link states in the JSON output.
const (
	stateOK        = "ok"
	stateBroken    = "broken"
	stateSkipped   = "skipped"
	stateUnchecked = "unchecked"
)

func linkState(l *crawl.Link) string {
	switch {
	case l.Skipped != "":
		return stateSkipped
	case !l.Checked:
		return stateUnchecked
	case l.Broken():
		return stateBroken
	default:
		return stateOK
	}
}

func writeJSON(w io.Writer, rep *crawl.Report) error {
	st := rep.Stats()
	doc := jsonReport{
		Start:       rep.Start,
		StartedAt:   rep.StartedAt.UTC(),
		DurationMS:  rep.Duration.Milliseconds(),
		Interrupted: rep.Interrupted,
		Summary: jsonStats{
			Pages: rep.Pages, Total: st.Total, OK: st.OK, Broken: st.Broken,
			Redirected: st.Redirected, Skipped: st.Skipped, Unchecked: st.Unchecked,
		},
		Links: make([]jsonLink, 0, len(rep.Links)),
	}
	for i := range rep.Links {
		l := &rep.Links[i]
		jl := jsonLink{
			URL:          l.URL,
			State:        linkState(l),
			Status:       l.Status,
			Error:        l.Error,
			SkipReason:   l.Skipped,
			Internal:     l.Internal,
			Page:         l.Page,
			Depth:        l.Depth,
			Method:       l.Method,
			Redirects:    l.Redirects,
			FinalURL:     l.FinalURL,
			Attempts:     l.Attempts,
			DurationMS:   l.Duration.Milliseconds(),
			FoundOnCount: l.SourceCount,
		}
		for _, s := range l.Sources {
			jl.FoundOn = append(jl.FoundOn, jsonSource(s))
		}
		doc.Links = append(doc.Links, jl)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}
