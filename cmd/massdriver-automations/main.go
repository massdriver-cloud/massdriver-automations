// massdriver-automations runs one Massdriver automation.
//
//	massdriver-automations list
//	massdriver-automations run <name>
//	massdriver-automations test <name> [args]
//
// AUTOMATION=<name> works in place of `run <name>`, for platforms that set only environment variables.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/automations"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 && os.Getenv("AUTOMATION") != "" {
		args = []string{"run", os.Getenv("AUTOMATION")}
	}

	switch {
	case len(args) == 1 && args[0] == "list":
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tEVENTS\tDESCRIPTION")
		for _, d := range automations.All {
			fmt.Fprintf(w, "%s\t%s\t%s\n", d.Name, strings.Join(d.Events, ","), d.Description)
		}
		_ = w.Flush()
	case len(args) == 2 && args[0] == "run":
		os.Exit(run(args[1]))
	case len(args) >= 2 && args[0] == "test":
		os.Exit(test(args[1], args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "usage: massdriver-automations list | run <name> | test <name> [args]")
		os.Exit(2)
	}
}

func run(name string) int {
	log, err := automation.NewLogger(os.Stderr, automation.Env("LOG_FORMAT", "auto"), automation.Env("LOG_LEVEL", "info"))
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("invalid log config",
			"automation", name, "version", version, "err", err)
		return 1
	}
	log = log.With("automation", name, "version", version)
	// SetDefault also sends the standard library's log package through this logger.
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := automation.StartTracing(ctx, name, version)
	if err != nil {
		log.Error("invalid tracing config", "err", err)
		return 1
	}
	if automation.TracingConfigured() {
		log.Info("tracing on")
	}
	defer func() {
		// ctx is already done at shutdown; the flush needs its own deadline.
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(fctx); err != nil {
			log.Warn("trace flush failed", "err", automation.RedactURL(err))
		}
	}()
	if err := serve(ctx, name); err != nil {
		var ce *automation.ConfigError
		if errors.As(err, &ce) {
			log.Error("invalid config", "err", err)
			return 1
		}
		log.Error("exit", "err", err)
		return 1
	}
	return 0
}

func test(name string, args []string) int {
	for _, d := range automations.All {
		if d.Name != name {
			continue
		}
		if d.Test == nil {
			fmt.Fprintf(os.Stderr, "%s has no test mode\n", name)
			return 2
		}
		if err := d.Test(args, os.Stdin, os.Stdout); err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(os.Stderr, err)
			}
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown automation %q; run `massdriver-automations list`\n", name)
	return 2
}

func serve(ctx context.Context, name string) error {
	for _, d := range automations.All {
		if d.Name != name {
			continue
		}
		a, err := d.New()
		if err != nil {
			return err
		}
		return automation.Serve(ctx, ":"+automation.Env("PORT", "8080"), a)
	}
	return fmt.Errorf("unknown automation %q; run `massdriver-automations list`", name)
}
