// Command edge renders Edge's configuration and confirms what an instance applied (Edge ADR 0002 and
// Edge ADR 0005):
//
//	edge render --instances <file> --installation <file> --fragments <dir> --contracts <dir>
//	  --secrets <dir> --revision <commit> --image <reference> --out <dir>
//	edge confirm --out <dir> --instance <address> --revision <commit> --internal <host:port>
//	  --control <host:port> --secrets <dir>
package main

import (
	"io"
	"os"
	"time"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
	"github.com/contelys/contelys-cloud-edge/internal/interfaces/commands"
)

// confirmation is how edge confirm reads an instance. Every answer over one polling interval of
// APISIX, one second, and the tenth of a second in which a worker process applies a changed file,
// must carry the distributed revision and digest; the answer is read every tenth of a second, each
// time on a connection of its own; and the instance counts as not applied after a minute (section 03).
var confirmation = application.Timing{Interval: time.Second + 100*time.Millisecond, Every: 100 * time.Millisecond, Timeout: time.Minute}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, infrastructure.DockerTrial{}, confirmation))
}

// run is the composition root: it hands the file-based infrastructure, the trial load and the
// instance's listeners to the use cases and starts the command line. The command's own tests hand it
// a trial load of their own, because the real one starts a container, which the scenarios exercise,
// and a shorter timing.
func run(args []string, stdout, stderr io.Writer, trial application.Trial, timing application.Timing) int {
	files := infrastructure.Files{}
	return commands.Run(args, stdout, stderr,
		application.Renderer{Source: files, Trial: trial, Sink: files}.Render,
		application.Confirmer{Distribution: files, Running: infrastructure.Endpoints{}, Timing: timing}.Confirm)
}
