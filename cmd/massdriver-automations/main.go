// massdriver-automations runs one Massdriver automation.
//
//	massdriver-automations list
//	massdriver-automations run <name>
//
// AUTOMATION=<name> works in place of `run <name>`, for platforms that set only environment variables.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/massdriver-cloud/massdriver-automations/automations"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

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
		if err := run(args[1]); err != nil {
			slog.Error("exit", "automation", args[1], "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: massdriver-automations list | run <name>")
		os.Exit(2)
	}
}

func run(name string) error {
	for _, d := range automations.All {
		if d.Name != name {
			continue
		}
		a, err := d.New()
		if err != nil {
			return err
		}
		addr := ":" + automation.Env("PORT", "8080")
		slog.Info("listening", "automation", name, "addr", addr)
		return automation.Serve(addr, name, a)
	}
	return fmt.Errorf("unknown automation %q; run `massdriver-automations list`", name)
}
