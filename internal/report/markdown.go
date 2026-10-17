package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/marcus-girolneto/linkpulse/internal/crawl"
)

const maxMarkdownSources = 3

func writeMarkdown(w io.Writer, rep *crawl.Report, opts Options) error {
	out := &errWriter{w: w}
	st := rep.Stats()

	out.printf("## linkpulse report\n\n")
	out.printf("Checked %s: %s crawled, %s checked in %s.\n\n",
		mdCode(rep.Start), plural(rep.Pages, "page", "pages"),
		plural(st.OK+st.Broken, "link", "links"), formatDuration(rep.Duration))

	out.printf("| Total | OK | Broken | Redirected | Skipped | Unchecked |\n")
	out.printf("|------:|---:|-------:|-----------:|--------:|----------:|\n")
	out.printf("| %d | %d | %d | %d | %d | %d |\n\n",
		st.Total, st.OK, st.Broken, st.Redirected, st.Skipped, st.Unchecked)

	if rep.Interrupted {
		out.printf("> [!WARNING]\n> The run was interrupted; results are partial.\n\n")
	}

	if b := broken(rep); len(b) > 0 {
		out.printf("### Broken links (%d)\n\n", len(b))
		out.printf("| Status | URL | Found on |\n")
		out.printf("|--------|-----|----------|\n")
		for _, l := range b {
			out.printf("| %s | %s | %s |\n", mdEscape(statusText(l)), mdCode(l.URL), mdSources(l))
		}
		out.printf("\n")
	} else if !rep.Interrupted {
		out.printf("No broken links found.\n\n")
	}

	if r := redirected(rep); opts.ShowRedirects && len(r) > 0 {
		out.printf("<details>\n<summary>Redirects (%d)</summary>\n\n", len(r))
		out.printf("| Status | URL | Target |\n")
		out.printf("|--------|-----|--------|\n")
		for _, l := range r {
			out.printf("| %d | %s | %s |\n", firstRedirectStatus(l), mdCode(l.URL), mdCode(finalTarget(l)))
		}
		out.printf("\n</details>\n")
	}
	return out.err
}

func mdSources(l *crawl.Link) string {
	if len(l.Sources) == 0 {
		return "(start URL)"
	}
	parts := make([]string, 0, maxMarkdownSources+1)
	for i, s := range l.Sources {
		if i == maxMarkdownSources {
			break
		}
		parts = append(parts, mdCode(s.Page))
	}
	if extra := l.SourceCount - len(parts); extra > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", extra))
	}
	return strings.Join(parts, "<br>")
}

func mdCode(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	if strings.Contains(s, "`") {
		return "`` " + s + " ``"
	}
	return "`" + s + "`"
}

func mdEscape(s string) string {
	r := strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;", "\n", " ")
	return r.Replace(s)
}
