package commands_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/interfaces/commands"
)

// The command line's exit codes (Edge ADR 0002), each with a failing fixture.

var complete = []string{"render",
	"--instances", "instances.yaml", "--installation", "installation.yaml", "--fragments", "fragments",
	"--secrets", "secrets", "--revision", "0123456789abcdef", "--out", "out",
}

func TestTheCommandLineReachesTheRendererAndEachOutcomeHasItsExitCode(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		outcome error
		code    int
		message string
	}{
		{"rendered", complete, nil, 0, ""},
		{"a refusal", complete, &application.Refusal{Fragment: "projects", Rule: "breaks the fragment schema"}, 1,
			"edge render: refused: fragment projects: breaks the fragment schema"},
		{"an error of the inputs", complete, errors.New("the instances file lists no instance of edge"), 2,
			"edge render: the instances file lists no instance of edge"},
		{"no command", nil, nil, 2, "usage: edge render"},
		{"another command", []string{"apply"}, nil, 2, "usage: edge render"},
		{"a missing flag", complete[:len(complete)-2], nil, 2, "edge render: --out is required"},
		{"an argument after the flags", append(append([]string{}, complete...), "extra"), nil, 2, "usage: edge render"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var received *application.Request
			render := func(req application.Request) error {
				received = &req
				return c.outcome
			}
			var stdout, stderr bytes.Buffer

			code := commands.Run(c.args, &stdout, &stderr, render)

			if code != c.code || !strings.Contains(stderr.String(), c.message) {
				t.Errorf("exit code %d, stderr %q; want %d and %q", code, stderr.String(), c.code, c.message)
			}
			reached := c.name == "rendered" || c.name == "a refusal" || c.name == "an error of the inputs"
			if reached != (received != nil) {
				t.Errorf("the renderer was called: %v, want %v", received != nil, reached)
			}
			if received != nil && (received.Instances != "instances.yaml" || received.Revision != "0123456789abcdef" || received.Out != "out") {
				t.Errorf("the renderer received %+v", *received)
			}
		})
	}
}
