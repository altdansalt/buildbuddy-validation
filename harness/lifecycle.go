package harness

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Stop gracefully stops a component, allowing up to 40 seconds for draining.
func (f *Fixture) Stop(c Component) { f.docker("stop", "--time", "40", f.Endpoint(c).Container) }

// Start starts an existing container and refreshes its endpoint in place.
// Previously dialed gRPC clients must be recreated after Start or Restart.
func (f *Fixture) Start(c Component) *Endpoint {
	ep := f.Endpoint(c)
	f.docker("start", ep.Container)
	return f.refresh(c)
}

func (f *Fixture) Restart(c Component) *Endpoint {
	f.docker("restart", "--time", "40", f.Endpoint(c).Container)
	return f.refresh(c)
}

// Kill sends a Docker signal without waiting for exit (TERM may initiate draining).
func (f *Fixture) Kill(c Component, signal string) {
	f.docker("kill", "--signal", signal, f.Endpoint(c).Container)
}

func (f *Fixture) refresh(c Component) *Endpoint {
	ep := f.Endpoint(c)
	ports := f.ports[c]
	ep.HTTP = "http://" + f.port(ep.Container, ports[0])
	if f.kinds[c] != Executor {
		ep.GRPC = f.port(ep.Container, ports[1])
	}
	f.waitReady(f.kinds[c], ep, f.opts.StartupTimeout)
	return ep
}

// AddExecutor creates another independently configurable executor. The returned
// component ID can be passed to Endpoint, Stop, Start, Restart, Kill and Exec.
// Options apply only to the new executor; nil fields inherit fixture options.
func (f *Fixture) AddExecutor(opts Options) Component {
	id := Component(fmt.Sprintf("executor-%d", len(f.Endpoints)))
	base := f.opts
	if opts.Images != nil {
		base.Images = opts.Images
	}
	if opts.Config != nil {
		base.Config = opts.Config
	}
	if opts.Args != nil {
		base.Args = opts.Args
	}
	if opts.DockerArgs != nil {
		base.DockerArgs = opts.DockerArgs
	}
	base.ExpectStartupFailure = opts.ExpectStartupFailure
	f.addComponent(id, Executor, base)
	return id
}

// WaitExit waits for exit within StartupTimeout, returning the exit code.
// unsuccessful additionally requires a nonzero exit code.
func (f *Fixture) WaitExit(c Component, unsuccessful bool) int {
	ep := f.Endpoint(c)
	deadline := time.Now().Add(f.opts.StartupTimeout)
	for time.Now().Before(deadline) {
		if f.docker("inspect", "--format", "{{.State.Running}}", ep.Container) == "false" {
			code, err := strconv.Atoi(f.docker("inspect", "--format", "{{.State.ExitCode}}", ep.Container))
			if err != nil {
				f.t.Fatal(err)
			}
			if unsuccessful && code == 0 {
				f.t.Fatalf("%s exited successfully, expected startup failure", c)
			}
			return code
		}
		time.Sleep(200 * time.Millisecond)
	}
	f.t.Fatalf("%s did not exit: %s", c, f.Logs(c))
	return -1
}

func (f *Fixture) Logs(c Component) string { return f.docker("logs", f.Endpoint(c).Container) }
func (f *Fixture) Exec(c Component, args ...string) string {
	return f.docker(append([]string{"exec", f.Endpoint(c).Container}, args...)...)
}

// CopyConfig replaces the config in a stopped component. Restart it to apply.
func (f *Fixture) CopyConfig(c Component, path string) {
	if strings.TrimSpace(path) == "" {
		f.t.Fatal("empty config path")
	}
	f.docker("cp", path, f.Endpoint(c).Container+":/config.yaml")
}
