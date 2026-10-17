package report

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/marcus-girolneto/linkpulse/internal/crawl"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

type painter bool

func (p painter) paint(code, s string) string {
	if !p || s == "" {
		return s
	}
	return code + s + ansiReset
}

// errWriter latches the first write error so rendering code can stay linear.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err == nil {
		_, e.err = fmt.Fprintf(e.w, format, args...)
	}
}

func writeText(w io.Writer, rep *crawl.Report, opts Options) error {
	out := &errWriter{w: w}
	p := painter(opts.Color)
	st := rep.Stats()

	out.printf("%s %s\n", p.paint(ansiBold, "linkpulse"), rep.Start)
	out.printf("%s\n\n", p.paint(ansiDim, fmt.Sprintf("%s crawled, %s checked in %s",
		plural(rep.Pages, "page", "pages"), plural(st.OK+st.Broken, "link", "links"), formatDuration(rep.Duration))))

	if b := broken(rep); len(b) > 0 {
		out.printf("%s\n\n", p.paint(ansiBold+ansiRed, fmt.Sprintf("Broken links (%d)", len(b))))
		rows := make([][]string, 0, len(b))
		for _, l := range b {
			rows = append(rows, []string{statusText(l), l.URL, foundOn(l)})
		}
		writeTable(out, p, []string{"STATUS", "URL", "FOUND ON"}, rows, 0, ansiRed)
		out.printf("\n")
	}

	if r := redirected(rep); opts.ShowRedirects && len(r) > 0 {
		out.printf("%s\n\n", p.paint(ansiBold+ansiYellow, fmt.Sprintf("Redirects (%d)", len(r))))
		rows := make([][]string, 0, len(r))
		for _, l := range r {
			rows = append(rows, []string{fmt.Sprint(firstRedirectStatus(l)), l.URL, finalTarget(l)})
		}
		writeTable(out, p, []string{"STATUS", "URL", "TARGET"}, rows, 0, ansiYellow)
		out.printf("\n")
	}

	if rep.Interrupted {
		out.printf("%s\n", p.paint(ansiYellow, fmt.Sprintf("Interrupted: partial results, %s not checked.",
			plural(st.Unchecked, "link", "links"))))
	}

	summary := fmt.Sprintf("%s: %d ok, %d broken, %d redirected, %d skipped",
		plural(st.Total, "link", "links"), st.OK, st.Broken, st.Redirected, st.Skipped)
	switch {
	case st.Broken > 0:
		out.printf("%s\n", p.paint(ansiRed, summary))
	case rep.Interrupted:
		out.printf("%s\n", summary)
	default:
		out.printf("%s\n", p.paint(ansiGreen, summary+". No broken links."))
	}
	return out.err
}

func foundOn(l *crawl.Link) string {
	switch {
	case len(l.Sources) == 0:
		return "(start URL)"
	case l.SourceCount > 1:
		return fmt.Sprintf("%s (+%d more)", l.Sources[0].Page, l.SourceCount-1)
	default:
		return l.Sources[0].Page
	}
}

// writeTable prints left-aligned columns. Widths are computed on the plain
// text so ANSI codes do not disturb alignment; colorCol is painted with
// color.
func writeTable(out *errWriter, p painter, header []string, rows [][]string, colorCol int, color string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	line := func(cells []string, style func(i int, s string) string) {
		var sb strings.Builder
		sb.WriteString("  ")
		for i, c := range cells {
			if i == len(cells)-1 {
				sb.WriteString(style(i, c))
				break
			}
			pad := widths[i] - utf8.RuneCountInString(c) + 2
			sb.WriteString(style(i, c))
			sb.WriteString(strings.Repeat(" ", pad))
		}
		out.printf("%s\n", sb.String())
	}
	line(header, func(_ int, s string) string { return p.paint(ansiDim, s) })
	for _, r := range rows {
		line(r, func(i int, s string) string {
			if i == colorCol {
				return p.paint(color, s)
			}
			return s
		})
	}
}
