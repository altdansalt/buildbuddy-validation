package configvalidation

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

var seed = flag.Int64("config.seed", 314, "Replay seed for configuration variations")
var iterations = flag.Int("config.iterations", 2, "Number of valid configuration fixtures (1..12)")

func TestValidConfigProperties(t *testing.T) {
	t.Logf("seed=%d iterations=%d", *seed, *iterations)
	if *iterations < 1 || *iterations > 12 {
		t.Fatal("config.iterations must be 1..12")
	}
	r := rand.New(rand.NewSource(*seed))
	for i := 0; i < *iterations; i++ {
		t.Run(fmt.Sprintf("seed_%d_iteration_%d", *seed, i), func(t *testing.T) {
			configs := map[harness.Component]harness.Config{}
			args := map[harness.Component][]string{}
			dockerArgs := map[harness.Component][]string{}
			for _, c := range components {
				// Independently generated YAML/CLI ports test precedence through readiness
				// and real RPC traffic, rather than inspecting the fixture's config map.
				configs[c] = harness.Config{"port": 10000 + r.Intn(10000)}
				if i%2 == 1 {
					args[c] = []string{fmt.Sprintf("--port=%d", 30000+r.Intn(10000))}
				}
				// String expansion occurs after typed YAML decoding. Verify a randomized
				// server identity via the readiness handler's server-type contract.
				identity := fmt.Sprintf("config-%s-%08x", c, r.Uint32())
				configs[c]["server_type"] = "${CONFIG_VALIDATION_ID}"
				args[c] = append(args[c], "--server_type="+identity)
				dockerArgs[c] = []string{"--env", "CONFIG_VALIDATION_ID=" + identity}
			}
			// CLI precedence for server_type masks YAML expansion, so test expansion
			// independently with a path observable through docker exec.
			root := fmt.Sprintf("/data/expanded-%08x", r.Uint32())
			configs[harness.Executor]["executor"] = harness.Config{"root_directory": "${CONFIG_VALIDATION_ROOT}"}
			dockerArgs[harness.Executor] = append(dockerArgs[harness.Executor], "--env", "CONFIG_VALIDATION_ROOT="+root)
			f := harness.Start(t, harness.Options{Components: components, Config: configs, Args: args, DockerArgs: dockerArgs})
			d := fixtureDocker(t, f)
			d.run(t, "exec", f.Endpoint(harness.Executor).Container, "test", "-d", root)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			payload := []byte(fmt.Sprintf("seed=%d iteration=%d value=%x", *seed, i, r.Uint64()))
			digest, err := f.Clients(harness.CacheProxy).Upload(ctx, "config-validation", payload)
			if err != nil {
				t.Fatalf("seed=%d upload: %v", *seed, err)
			}
			got, err := f.Clients(harness.App).Read(ctx, "config-validation", digest)
			if err != nil || string(got) != string(payload) {
				t.Fatalf("seed=%d read=%q err=%v", *seed, got, err)
			}
		})
	}
}

func TestInvalidTypesAndPortRange(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: components})
	d := fixtureDocker(t, f)
	for _, c := range components {
		original := baseline(t, f, c)
		for _, value := range []string{"not-an-integer", "-2", "65536"} {
			t.Run(fmt.Sprintf("%s_port_%s", c, value), func(t *testing.T) {
				since := restartConfig(t, f, d, c, portLine.ReplaceAllString(original, "port: "+value))
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) && d.run(t, "inspect", "--format", "{{.State.Running}}", f.Endpoint(c).Container) == "true" {
					time.Sleep(200 * time.Millisecond)
				}
				running := d.run(t, "inspect", "--format", "{{.State.Running}}", f.Endpoint(c).Container)
				logs := d.run(t, "logs", "--since", since, f.Endpoint(c).Container)
				t.Logf("%s port=%s startup logs:\n%s", c, value, logs)
				if running == "true" {
					t.Errorf("invalid port=%s did not stop startup", value)
				}
				if !strings.Contains(strings.ToLower(logs), "port") {
					t.Errorf("error does not identify port")
				}
				if strings.Contains(logs, "panic:") {
					t.Errorf("startup panicked")
				}
			})
		}
		// Restore peers before testing the next component.
		restartConfig(t, f, d, c, original)
		client := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(time.Minute)
		ready := false
		for time.Now().Before(deadline) {
			resp, err := client.Get(f.Endpoint(c).HTTP + "/readyz?server-type=" + string(c))
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					ready = true
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !ready {
			t.Fatalf("restored baseline %s did not become ready", c)
		}
	}
}

func TestExecutorRequiredTargetAndTrafficRange(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor}})
	d := fixtureDocker(t, f)
	original := baseline(t, f, harness.Executor)
	cases := []struct{ name, config, setting string }{
		{"missing_app_target", regexp.MustCompile(`(?m)^    app_target: [^\n]+$`).ReplaceAllString(original, `    app_target: ""`), "app target"},
		{"traffic_below_range", original + "\nexecutor.cache_target_traffic_percent: -2\n", "executor.cache_target_traffic_percent"},
		{"traffic_above_range", original + "\nexecutor.cache_target_traffic_percent: 101\n", "executor.cache_target_traffic_percent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Insert the range option into the existing executor mapping; dotted YAML
			// keys are not the supported nesting syntax.
			config := tc.config
			if tc.name != "missing_app_target" {
				value := "-2"
				if tc.name == "traffic_above_range" {
					value = "101"
				}
				config = strings.Replace(original, "executor:\n", "executor:\n    cache_target_traffic_percent: "+value+"\n", 1)
			}
			if config == original {
				t.Fatal("configuration mutation did not match baseline")
			}
			since := restartConfig(t, f, d, harness.Executor, config)
			name := f.Endpoint(harness.Executor).Container
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) && d.run(t, "inspect", "--format", "{{.State.Running}}", name) == "true" {
				time.Sleep(200 * time.Millisecond)
			}
			logs := d.run(t, "logs", "--since", since, name)
			t.Logf("%s logs:\n%s", tc.name, logs)
			if d.run(t, "inspect", "--format", "{{.State.Running}}", name) == "true" {
				t.Error("invalid executor config remained running")
			}
			if !strings.Contains(logs, tc.setting) {
				t.Errorf("startup error must name %s", tc.setting)
			}
			if strings.Contains(logs, "panic:") {
				t.Error("startup panicked")
			}
		})
	}
}
