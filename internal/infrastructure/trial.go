package infrastructure

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/ancyloce/contelys-cloud-edge/internal/application"
)

// DockerTrial implements application.Trial with the Docker engine that DOCKER_HOST names, or else
// the local one (section 03). It creates a container of the candidate's image, copies the candidate
// files into it where an Edge instance mounts them, starts it, reads the ids of the routes and
// upstreams that its control listener reports, and removes it. The files are copied rather than
// mounted, so that the renderer can itself run in a container. The control listener's port is
// published on the loopback address of the Docker host, where the renderer reaches it.
type DockerTrial struct{}

// Where an Edge instance reads its files: config.yaml in APISIX's own place, and apisix.yaml in the
// directory that the image links conf/apisix.yaml into (Edge ADR 0003).
const (
	configPath = "/usr/local/apisix/conf/config.yaml"
	routesPath = "/etc/edge/routes/apisix.yaml"
)

// The limits of a trial load: how long the instance may take to start and answer, how long one
// request to its control listener may take, how often it is asked, how long the removal of the
// container may take, and how long reading its last log lines may take.
const (
	trialTimeout          = 60 * time.Second
	controlRequestTimeout = 2 * time.Second
	pollInterval          = 100 * time.Millisecond
	removeTimeout         = 10 * time.Second
	logsTimeout           = 10 * time.Second
)

// TrialLabel marks the containers of the trial load, so that an operator can find one that a
// killed renderer left behind.
const TrialLabel = "contelys.edge.trial-load"

// Load implements application.Trial. It encodes and checks the files as the sink does, so a file
// that breaks a rule of its bytes fails with the same message, before any container starts.
func (DockerTrial) Load(c application.Candidate) (application.Loaded, error) {
	routes, err := encodeRouteFile(routeFileName(c.Address), c.File)
	if err != nil {
		return application.Loaded{}, err
	}
	config, err := encodeConfigFile(c.Config)
	if err != nil {
		return application.Loaded{}, err
	}
	fail := func(format string, a ...any) (application.Loaded, error) {
		return application.Loaded{}, fmt.Errorf("the trial load: "+format, a...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), trialTimeout)
	defer cancel()
	docker, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fail("%v", err)
	}
	defer docker.Close()

	control, err := network.ParsePort(fmt.Sprintf("%d/tcp", c.Config.Apisix.Control.Port))
	if err != nil {
		return fail("the control listener's port: %v", err)
	}
	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        c.Image,
			ExposedPorts: network.PortSet{control: {}},
			Labels:       map[string]string{TrialLabel: "true"},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{control: {{HostIP: netip.MustParseAddr("127.0.0.1")}}},
		},
	})
	if err != nil {
		return fail("create a container of %s: %v", c.Image, err)
	}
	defer func() {
		// The removal outlives a trial load that ran out of time.
		ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
		defer cancel()
		docker.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	files, err := archive(map[string][]byte{
		configPath:                 config,
		routesPath:                 routes,
		application.TrustStorePath: []byte(c.TrustStore),
	})
	if err != nil {
		return fail("%v", err)
	}
	if _, err := docker.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: files}); err != nil {
		return fail("copy the candidate files: %v", err)
	}
	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fail("start the container: %v", err)
	}
	inspected, err := docker.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		return fail("inspect the container: %v", err)
	}
	bindings := inspected.Container.NetworkSettings.Ports[control]
	if len(bindings) == 0 {
		return fail("the control listener's port %s is not published", control)
	}
	base := "http://" + net.JoinHostPort(bindings[0].HostIP.String(), bindings[0].HostPort)

	// Every worker process loads the whole file while it starts, so the first answer of the control
	// listener already reports everything that the instance loaded.
	controlClient := &http.Client{Timeout: controlRequestTimeout}
	var loaded application.Loaded
	for {
		loaded.Routes, err = loadedIDs(ctx, controlClient, base+"/v1/routes")
		if err == nil {
			break
		}
		state, inspectErr := docker.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
		if inspectErr == nil && state.Container.State != nil && !state.Container.State.Running {
			return fail("the instance stopped with exit code %d: %s", state.Container.State.ExitCode, logsOf(docker, created.ID))
		}
		select {
		case <-ctx.Done():
			return fail("the instance did not answer on its control listener within %v (%v): %s", trialTimeout, err, logsOf(docker, created.ID))
		case <-time.After(pollInterval):
		}
	}
	if loaded.Upstreams, err = loadedIDs(ctx, controlClient, base+"/v1/upstreams"); err != nil {
		return fail("%v", err)
	}
	return loaded, nil
}

// archive is a tar archive of files, each under its absolute path in the container. Each file
// carries the current time as its modification time: APISIX uses that time as the version of every
// resource, and never applies a file whose time is 0 (section 03).
func archive(files map[string][]byte) (io.Reader, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	now := time.Now()
	for path, content := range files {
		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     strings.TrimPrefix(path, "/"),
			Mode:     0o644,
			Size:     int64(len(content)),
			ModTime:  now,
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(content); err != nil {
			return nil, err
		}
	}
	return &buffer, writer.Close()
}

// loadedIDs reads the ids of the loaded routes or upstreams from the control listener.
func loadedIDs(ctx context.Context, c *http.Client, url string) ([]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", url, response.StatusCode)
	}
	var items []struct {
		Value struct {
			ID any `json:"id"`
		} `json:"value"`
	}
	// APISIX encodes an empty list as an empty object, or answers with no body at all.
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("%s: %w", url, err)
		}
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = fmt.Sprint(item.Value.ID)
	}
	return ids, nil
}

// logsOf returns the last lines that the container wrote, to explain why the trial load failed.
func logsOf(docker *client.Client, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), logsTimeout)
	defer cancel()
	stream, err := docker.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "20"})
	if err != nil {
		return "no logs: " + err.Error()
	}
	defer stream.Close()
	var lines bytes.Buffer
	if _, err := stdcopy.StdCopy(&lines, &lines, stream); err != nil && !errors.Is(err, io.EOF) {
		return "no logs: " + err.Error()
	}
	return strings.TrimSpace(lines.String())
}
