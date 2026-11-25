// massdriver-automations runs one Massdriver automation.
//
//	massdriver-automations list
//	massdriver-automations run <name>
//
// AUTOMATION=<name> works in place of `run <name>`, for platforms that set only environment variables.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

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
		w.Flush()
	case len(args) == 2 && args[0] == "run":
		os.Exit(run(args[1]))
	default:
		fmt.Fprintln(os.Stderr, "usage: massdriver-automations list | run <name>")
		os.Exit(2)
	}
}

func run(name string) int {
	log, err := automation.NewLogger(os.Stderr, automation.Env("LOG_FORMAT", "json"), automation.Env("LOG_LEVEL", "info"))
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
	if err := serve(ctx, name); err != nil {
		log.Error("exit", "err", err)
		return 1
	}
	return 0
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
