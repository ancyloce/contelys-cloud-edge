// Package commands reads the command line of the edge command and reports its refusals.
package commands

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// The exit codes of the command line (Edge ADR 0002).
const (
	Rendered = 0
	Refused  = 1
	Failed   = 2
)

const usage = "usage: edge render --instances <file> --installation <file> --fragments <dir> --secrets <dir> --revision <commit> --out <dir>"

// Run runs one command and returns its exit code.
func Run(args []string, stdout, stderr io.Writer, renderer application.Renderer) int {
	if len(args) == 0 || args[0] != "render" {
		fmt.Fprintln(stderr, usage)
		return Failed
	}
	var req application.Request
	flags := flag.NewFlagSet("render", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, usage) }
	flags.StringVar(&req.Instances, "instances", "", "the instances file")
	flags.StringVar(&req.Installation, "installation", "", "the installation file")
	flags.StringVar(&req.Fragments, "fragments", "", "the directory of the route fragments")
	flags.StringVar(&req.Secrets, "secrets", "", "the directory of the secret files")
	flags.StringVar(&req.Revision, "revision", "", "the configuration revision, a Git commit")
	flags.StringVar(&req.Out, "out", "", "the output directory")
	if err := flags.Parse(args[1:]); err != nil {
		return Failed
	}
	missing := false
	flags.VisitAll(func(f *flag.Flag) {
		if f.Value.String() == "" {
			fmt.Fprintf(stderr, "edge render: --%s is required\n", f.Name)
			missing = true
		}
	})
	if missing || flags.NArg() > 0 {
		fmt.Fprintln(stderr, usage)
		return Failed
	}

	err := renderer.Render(req)
	var refusal *application.Refusal
	switch {
	case err == nil:
		return Rendered
	case errors.As(err, &refusal):
		fmt.Fprintf(stderr, "edge render: refused: %v\n", err)
		return Refused
	default:
		fmt.Fprintf(stderr, "edge render: %v\n", err)
		return Failed
	}
}
