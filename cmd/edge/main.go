// Command edge renders Edge's configuration: edge render --instances <file> --installation <file>
// --fragments <dir> --contracts <dir> --secrets <dir> --revision <commit> --image <reference>
// --out <dir> (Edge ADR 0002).
package main

import (
	"io"
	"os"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
	"github.com/contelys/contelys-cloud-edge/internal/interfaces/commands"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, infrastructure.DockerTrial{}))
}

// run is the composition root: it hands the file-based infrastructure and the trial load to the use
// case and starts the command line. The command's own tests hand it a trial load of their own,
// because the real one starts a container, which the scenarios exercise.
func run(args []string, stdout, stderr io.Writer, trial application.Trial) int {
	files := infrastructure.Files{}
	return commands.Run(args, stdout, stderr, application.Renderer{Source: files, Trial: trial, Sink: files}.Render)
}
