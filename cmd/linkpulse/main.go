// Command linkpulse is a fast, concurrent broken-link checker.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/marcus-girolneto/linkpulse/internal/cli"
)

// Set at build time via -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal, restore default handling so a second
		// Ctrl-C terminates immediately instead of waiting for the report.
		<-ctx.Done()
		stop()
	}()

	code := cli.Run(ctx, os.Args[1:], cli.Env{
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		StdoutTTY: isTerminal(os.Stdout),
		StderrTTY: isTerminal(os.Stderr),
		Getenv:    os.Getenv,
		Version:   version,
		Commit:    commit,
		Date:      date,
	})
	stop()
	os.Exit(code)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
