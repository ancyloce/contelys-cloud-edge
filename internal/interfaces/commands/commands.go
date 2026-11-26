// Package commands reads the command line of the edge command and reports its refusals.
package commands

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ancyloce/contelys-cloud-edge/internal/application"
)

// The exit codes of the command line (Edge ADR 0002 and Edge ADR 0005).
const (
	// Rendered, or confirmed.
	Rendered = 0
	// Refused: a route fragment breaks a rule, or the instance has not applied its distributed file.
	Refused = 1
	// Failed: a usage error, or a file, an instance or a trial load that cannot be read or run.
	Failed = 2
)

const usage = `usage: edge render --instances <file> --installation <file> --fragments <dir> --contracts <dir> --secrets <dir> --revision <commit> --image <reference> --out <dir>
       edge confirm --out <dir> --instance <address> --revision <commit> --internal <host:port> --control <host:port> --secrets <dir>`

// Run runs one command and returns its exit code. render is the use case behind edge render, and
// confirm the one behind edge confirm.
func Run(args []string, stdout, stderr io.Writer, render func(application.Request) error, confirm func(application.ConfirmRequest) (application.Answer, error)) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return Failed
	}
	switch args[0] {
	case "render":
		var req application.Request
		flags := newFlags("render", stderr)
		flags.StringVar(&req.Instances, "instances", "", "the instances file")
		flags.StringVar(&req.Installation, "installation", "", "the installation file")
		flags.StringVar(&req.Fragments, "fragments", "", "the directory of the route fragments")
		flags.StringVar(&req.Contracts, "contracts", "", "the directory of the OpenAPI contracts, openapi/ of the shared contracts")
		flags.StringVar(&req.Secrets, "secrets", "", "the directory of the secret files")
		flags.StringVar(&req.Revision, "revision", "", "the configuration revision, a Git commit")
		flags.StringVar(&req.Image, "image", "", "Edge's image, which the trial load starts")
		flags.StringVar(&req.Out, "out", "", "the output directory")
		if !parse(flags, args[1:], stderr) {
			return Failed
		}
		return outcome("render", render(req), stderr)
	case "confirm":
		var req application.ConfirmRequest
		flags := newFlags("confirm", stderr)
		flags.StringVar(&req.Out, "out", "", "the output directory of edge render")
		flags.StringVar(&req.Instance, "instance", "", "the Edge instance's address in the instances file")
		flags.StringVar(&req.Revision, "revision", "", "the configuration revision that the instance must have applied")
		flags.StringVar(&req.Internal, "internal", "", "the instance's internal listener, as host:port")
		flags.StringVar(&req.Control, "control", "", "the instance's control listener, as host:port")
		flags.StringVar(&req.Secrets, "secrets", "", "the directory of the secret files, whose trust store verifies the internal listener")
		if !parse(flags, args[1:], stderr) {
			return Failed
		}
		answer, err := confirm(req)
		if err == nil {
			fmt.Fprintf(stdout, "edge confirm: %s applied revision %s with digest %s\n", req.Instance, answer.Revision, answer.Digest)
		}
		return outcome("confirm", err, stderr)
	default:
		fmt.Fprintln(stderr, usage)
		return Failed
	}
}

func newFlags(command string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, usage) }
	return flags
}

// parse reads the flags, every one of which is required, and reports a usage error.
func parse(flags *flag.FlagSet, args []string, stderr io.Writer) bool {
	if err := flags.Parse(args); err != nil {
		return false
	}
	missing := false
	flags.VisitAll(func(f *flag.Flag) {
		if f.Value.String() == "" {
			fmt.Fprintf(stderr, "edge %s: --%s is required\n", flags.Name(), f.Name)
			missing = true
		}
	})
	if missing || flags.NArg() > 0 {
		fmt.Fprintln(stderr, usage)
		return false
	}
	return true
}

// outcome reports the result of a command and returns its exit code.
func outcome(command string, err error, stderr io.Writer) int {
	var refusal *application.Refusal
	var notApplied *application.NotApplied
	switch {
	case err == nil:
		return Rendered
	case errors.As(err, &refusal):
		fmt.Fprintf(stderr, "edge %s: refused: %v\n", command, err)
		return Refused
	case errors.As(err, &notApplied):
		fmt.Fprintf(stderr, "edge %s: %v\n", command, err)
		return Refused
	default:
		fmt.Fprintf(stderr, "edge %s: %v\n", command, err)
		return Failed
	}
}
