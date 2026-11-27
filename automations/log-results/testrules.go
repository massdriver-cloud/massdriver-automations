package logresults

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// Test runs the rules of a config file against a saved log and prints what the automation would write. It calls no
// API, so people can write and check rules without a deployment.
//
//	massdriver-automations test log-results [-config FILE] [-action PLAN] [-failed] [-attr KEY=VALUE]... [LOG]
//
// Without LOG it reads standard input.
func Test(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("test log-results", flag.ContinueOnError)
	fs.SetOutput(stdout)
	configFile := fs.String("config", automation.ConfigPath(), "the config file")
	action := fs.String("action", "PROVISION", "the deployment action: PROVISION, PLAN, or DECOMMISSION")
	failed := fs.Bool("failed", false, "read the log as the log of a failed deployment")
	var attrs attrFlags
	fs.Var(&attrs, "attr", "an attribute of the deployment, such as md-project=api; repeat for more")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("want at most one log file, got %d", fs.NArg())
	}

	var cfg Config
	if err := automation.LoadConfigFile(*configFile, &cfg); err != nil {
		return err
	}
	in := stdin
	if fs.NArg() == 1 {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}

	d := Deployment{ID: "local", Action: strings.ToUpper(*action), Failed: *failed, Attributes: attrs.m}
	entries, skips, err := cfg.Parse(context.Background(), io.LimitReader(in, cfg.maxLogBytes()), d)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RULE\tKEY\tFORMAT\tLABEL\tVALUE")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Rule, e.Key, e.Format, e.Label, e.Value)
	}
	for _, s := range skips {
		why := strings.ReplaceAll(s.Reason, "_", " ")
		if s.Detail != "" {
			why += ": " + s.Detail
		}
		fmt.Fprintf(w, "%s\t-\t-\t-\tskipped: %s\n", s.Rule, why)
	}
	return w.Flush()
}

type attrFlags struct{ m map[string]any }

func (a *attrFlags) String() string { return "" }

func (a *attrFlags) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value")
	}
	if a.m == nil {
		a.m = map[string]any{}
	}
	a.m[k] = v
	return nil
}
