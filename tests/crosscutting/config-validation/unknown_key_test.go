package configvalidation

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

func TestUnknownKeyWarnsAndStarts(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: components})
	d := fixtureDocker(t, f)
	for _, c := range components {
		t.Run(string(c), func(t *testing.T) {
			// Minimal typo of the common monitoring_port setting.
			since := restartConfig(t, f, d, c, baseline(t, f, c)+"\nmonitoring_prt: 9191\n")
			client := &http.Client{Timeout: time.Second}
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := client.Get(f.Endpoint(c).HTTP + "/readyz?server-type=" + string(c))
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						logs := d.run(t, "logs", "--since", since, f.Endpoint(c).Container)
						if !strings.Contains(logs, "No flags correspond to YAML input at 'monitoring_prt'.") || strings.Contains(logs, "panic:") {
							t.Fatalf("unknown key must warn without panic:\n%s", logs)
						}
						return
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
			t.Fatalf("component did not become ready after ignoring unknown key\n%s", d.run(t, "logs", "--since", since, f.Endpoint(c).Container))
		})
	}
}
