// Package cli implements the linkpulse command.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/check"
	"github.com/marcus-girolneto/linkpulse/internal/crawl"
	"github.com/marcus-girolneto/linkpulse/internal/hostlimit"
	"github.com/marcus-girolneto/linkpulse/internal/report"
	"github.com/marcus-girolneto/linkpulse/internal/robots"
	"github.com/marcus-girolneto/linkpulse/internal/urlnorm"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

// Env is everything Run needs from the outside world.
type Env struct {
	Stdout, Stderr       io.Writer
	StdoutTTY, StderrTTY bool
	Getenv               func(string) string
	Version              string
	Commit               string
	Date                 string
}

type options struct {
	depth           int
	maxPages        int
	workers         int
	hostConcurrency int
	rate            float64
	timeout         time.Duration
	retries         int
	maxRedirects    int
	include         regexpList
	exclude         regexpList
	noRobots        bool
	internalOnly    bool
	userAgent       string
	format          string
	output          string
	showRedirects   bool
	failOn          string
	color           string
	quiet           bool
	version         bool
}

type regexpList []*regexp.Regexp

func (r *regexpList) String() string {
	parts := make([]string, len(*r))
	for i, re := range *r {
		parts[i] = re.String()
	}
	return strings.Join(parts, ",")
}

func (r *regexpList) Set(s string) error {
	re, err := regexp.Compile(s)
	if err != nil {
		return err
	}
	*r = append(*r, re)
	return nil
}

const usageHeader = `linkpulse checks a website for broken links.

Usage:
  linkpulse [flags] <url>

It crawls pages on the same host as <url>, checks every link it finds
(internal and external) and prints the broken ones with the pages they
appear on.

Exit codes:
  0    no failures (according to --fail-on)
  1    failures found (according to --fail-on)
  2    usage or setup error
  130  interrupted; a partial report is still printed

Flags:
`

func newFlagSet(o *options, stderr io.Writer, version string) *flag.FlagSet {
	fs := flag.NewFlagSet("linkpulse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		// show --flags instead of -flags
		var buf strings.Builder
		fs.SetOutput(&buf)
		fs.PrintDefaults()
		fs.SetOutput(stderr)
		fmt.Fprint(stderr, usageHeader)
		fmt.Fprint(stderr, strings.ReplaceAll("\n"+buf.String(), "\n  -", "\n  --")[1:])
	}
	fs.IntVar(&o.depth, "depth", 5, "maximum link depth to crawl from the start page (0 = start page only, -1 = unlimited)")
	fs.IntVar(&o.maxPages, "max-pages", 1000, "maximum number of pages to fetch and parse (0 = unlimited)")
	fs.IntVar(&o.workers, "workers", 32, "number of concurrent checks across all hosts")
	fs.IntVar(&o.hostConcurrency, "host-concurrency", 4, "maximum concurrent requests per host")
	fs.Float64Var(&o.rate, "rate", 10, "maximum requests per second per host (0 = unlimited)")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "timeout for a single request attempt")
	fs.IntVar(&o.retries, "retries", 2, "retries for transient failures (timeouts, resets, 429, 502-504)")
	fs.IntVar(&o.maxRedirects, "max-redirects", 10, "maximum redirects to follow per link")
	fs.Var(&o.include, "include", "only crawl pages whose URL matches `regex` (repeatable); links on crawled pages are still checked")
	fs.Var(&o.exclude, "exclude", "skip URLs matching `regex` entirely (repeatable)")
	fs.BoolVar(&o.noRobots, "no-robots", false, "ignore robots.txt and meta robots nofollow")
	fs.BoolVar(&o.internalOnly, "internal-only", false, "do not check links to other hosts")
	fs.StringVar(&o.userAgent, "user-agent", "linkpulse/"+version+" (+https://github.com/marcus-girolneto/linkpulse)", "User-Agent header to send")
	fs.StringVar(&o.format, "format", "text", "output format: text, json or markdown")
	fs.StringVar(&o.output, "output", "", "write the report to `file` instead of stdout")
	fs.BoolVar(&o.showRedirects, "show-redirects", false, "list redirected links in text and markdown output")
	fs.StringVar(&o.failOn, "fail-on", "broken", "exit with status 1 on: broken, redirect (broken or redirected) or none")
	fs.StringVar(&o.color, "color", "auto", "colorize output: auto, always or never (NO_COLOR is honored in auto)")
	fs.BoolVar(&o.quiet, "quiet", false, "do not print progress to stderr")
	fs.BoolVar(&o.version, "version", false, "print version information and exit")
	return fs
}

// parseArgs parses flags that may appear before or after the URL.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// Run executes the command and returns the process exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Version == "" {
		env.Version = "dev"
	}
	var o options
	fs := newFlagSet(&o, env.Stderr, env.Version)
	positional, err := parseArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		return ExitUsage
	}
	if o.version {
		fmt.Fprintf(env.Stdout, "linkpulse %s (commit %s, built %s)\n", env.Version, orNone(env.Commit), orNone(env.Date))
		return ExitOK
	}
	cfg, err := o.validate(positional, env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "linkpulse: %v\n\nRun 'linkpulse --help' for usage.\n", err)
		return ExitUsage
	}
	return run(ctx, cfg, env)
}

type runConfig struct {
	options
	start     *url.URL
	format    report.Format
	colorOut  bool
	progress  bool
	failCheck func(crawl.Stats) bool
}

func (o *options) validate(positional []string, env Env) (*runConfig, error) {
	if len(positional) != 1 {
		return nil, errors.New("expected exactly one URL argument")
	}
	raw := positional[0]
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	start, err := urlnorm.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", positional[0], err)
	}
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return nil, err
	}
	var failCheck func(crawl.Stats) bool
	switch o.failOn {
	case "broken":
		failCheck = func(s crawl.Stats) bool { return s.Broken > 0 }
	case "redirect":
		failCheck = func(s crawl.Stats) bool { return s.Broken > 0 || s.Redirected > 0 }
	case "none":
		failCheck = func(crawl.Stats) bool { return false }
	default:
		return nil, fmt.Errorf("invalid --fail-on %q (want broken, redirect or none)", o.failOn)
	}
	toStdout := o.output == "" || o.output == "-"
	var colorOut bool
	switch o.color {
	case "always":
		colorOut = true
	case "never":
	case "auto":
		colorOut = toStdout && env.StdoutTTY && env.Getenv("NO_COLOR") == "" && env.Getenv("TERM") != "dumb"
	default:
		return nil, fmt.Errorf("invalid --color %q (want auto, always or never)", o.color)
	}
	switch {
	case o.workers < 1:
		return nil, errors.New("--workers must be at least 1")
	case o.hostConcurrency < 1:
		return nil, errors.New("--host-concurrency must be at least 1")
	case o.rate < 0:
		return nil, errors.New("--rate must not be negative")
	case o.timeout <= 0:
		return nil, errors.New("--timeout must be positive")
	case o.retries < 0:
		return nil, errors.New("--retries must not be negative")
	case o.maxRedirects < 1:
		return nil, errors.New("--max-redirects must be at least 1")
	}
	return &runConfig{
		options:   *o,
		start:     start,
		format:    format,
		colorOut:  colorOut,
		progress:  !o.quiet && env.StderrTTY,
		failCheck: failCheck,
	}, nil
}

func run(ctx context.Context, cfg *runConfig, env Env) int {
	start := cfg.start
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = cfg.hostConcurrency
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	limiter := hostlimit.New(hostlimit.Config{Rate: cfg.rate, Burst: cfg.hostConcurrency, Concurrency: cfg.hostConcurrency})
	checker := check.New(client, limiter, check.Config{
		UserAgent:    cfg.userAgent,
		Timeout:      cfg.timeout,
		Retries:      cfg.retries,
		MaxRedirects: cfg.maxRedirects,
	})

	crawlCfg := crawl.Config{
		MaxDepth:     cfg.depth,
		MaxPages:     cfg.maxPages,
		Workers:      cfg.workers,
		Include:      cfg.include,
		Exclude:      cfg.exclude,
		SkipExternal: cfg.internalOnly,
	}
	if !cfg.noRobots {
		rctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		rules, err := robots.Fetch(rctx, client, start, cfg.userAgent)
		cancel()
		if err != nil {
			fmt.Fprintf(env.Stderr, "linkpulse: warning: %v\n", err)
		}
		if d := rules.CrawlDelay(); d > 0 {
			limiter.SlowDown(start.Host, d)
			fmt.Fprintf(env.Stderr, "linkpulse: honoring robots.txt Crawl-delay of %s for %s\n", d, start.Host)
		}
		crawlCfg.Robots = rules
	}

	var prog *progress
	if cfg.progress {
		prog = newProgress(env.Stderr)
		crawlCfg.OnResult = prog.observe
		prog.start()
	}
	rep := crawl.New(checker, crawlCfg).Run(ctx, start)
	if prog != nil {
		prog.stop()
	}

	if err := writeReport(cfg, rep, env); err != nil {
		fmt.Fprintf(env.Stderr, "linkpulse: %v\n", err)
		return ExitUsage
	}

	switch {
	case rep.Interrupted:
		return ExitInterrupted
	case cfg.failCheck(rep.Stats()):
		return ExitFailed
	default:
		return ExitOK
	}
}

func writeReport(cfg *runConfig, rep *crawl.Report, env Env) (err error) {
	opts := report.Options{Color: cfg.colorOut, ShowRedirects: cfg.showRedirects}
	if cfg.output == "" || cfg.output == "-" {
		return report.Write(env.Stdout, rep, cfg.format, opts)
	}
	f, err := os.Create(cfg.output)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close output: %w", cerr)
		}
	}()
	if err := report.Write(f, rep, cfg.format, opts); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
