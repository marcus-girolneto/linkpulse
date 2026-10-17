// Package report renders crawl results as a terminal table, JSON or
// Markdown.
package report

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/crawl"
)

// Format selects an output renderer.
type Format string

// Supported formats.
const (
	Text     Format = "text"
	JSON     Format = "json"
	Markdown Format = "markdown"
)

// ErrUnknownFormat is returned by ParseFormat for unsupported names.
var ErrUnknownFormat = errors.New("unknown format")

// ParseFormat converts a user-supplied name into a Format.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "text", "table", "":
		return Text, nil
	case "json":
		return JSON, nil
	case "markdown", "md":
		return Markdown, nil
	}
	return "", fmt.Errorf("%w %q (want text, json or markdown)", ErrUnknownFormat, s)
}

// Options tune rendering.
type Options struct {
	// Color enables ANSI colors in the text format.
	Color bool
	// ShowRedirects lists redirected links in text and Markdown output.
	ShowRedirects bool
}

// Write renders rep to w in the given format.
func Write(w io.Writer, rep *crawl.Report, format Format, opts Options) error {
	switch format {
	case Text:
		return writeText(w, rep, opts)
	case JSON:
		return writeJSON(w, rep)
	case Markdown:
		return writeMarkdown(w, rep, opts)
	}
	return fmt.Errorf("%w %q", ErrUnknownFormat, format)
}

// statusText is the short outcome shown in the status column.
func statusText(l *crawl.Link) string {
	if l.Error != "" {
		return l.Error
	}
	return fmt.Sprint(l.Status)
}

func broken(rep *crawl.Report) []*crawl.Link {
	var out []*crawl.Link
	for i := range rep.Links {
		if rep.Links[i].Broken() {
			out = append(out, &rep.Links[i])
		}
	}
	return out
}

func redirected(rep *crawl.Report) []*crawl.Link {
	var out []*crawl.Link
	for i := range rep.Links {
		if rep.Links[i].Redirected() {
			out = append(out, &rep.Links[i])
		}
	}
	return out
}

func finalTarget(l *crawl.Link) string {
	if l.FinalURL != "" {
		return l.FinalURL
	}
	return l.URL
}

func firstRedirectStatus(l *crawl.Link) int {
	if len(l.Redirects) == 0 {
		return 0
	}
	return l.Redirects[0].Status
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(10 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
