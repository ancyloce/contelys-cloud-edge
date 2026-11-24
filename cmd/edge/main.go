// Command edge renders Edge's configuration: edge render --instances <file> --installation <file>
// --fragments <dir> --secrets <dir> --revision <commit> --out <dir> (Edge ADR 0002).
package main

import (
	"io"
	"os"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
	"github.com/contelys/contelys-cloud-edge/internal/interfaces/commands"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the composition root: it hands the file-based infrastructure to the use case and starts
// the command line.
func run(args []string, stdout, stderr io.Writer) int {
	files := infrastructure.Files{}
	return commands.Run(args, stdout, stderr, application.Renderer{Source: files, Sink: files})
}
