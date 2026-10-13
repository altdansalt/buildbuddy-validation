package configvalidation

import (
	"net/http"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
)

func TestUnknownKeyMustRejectStartup(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: components})
	d := fixtureDocker(t, f)
	for _, c := range components {
		t.Run(string(c), func(t *testing.T) {
			// Minimal typo of the common monitoring_port setting.
			since := restartConfig(t, f, d, c, baseline(t, f, c)+"\nmonitoring_prt: 9191\n")
			client := &http.Client{Timeout: time.Second}
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if d.run(t, "inspect", "--format", "{{.State.Running}}", f.Endpoint(c).Container) != "true" {
					return
				}
				resp, err := client.Get(f.Endpoint(c).HTTP + "/readyz?server-type=" + string(c))
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						t.Fatalf("unknown monitoring_prt accepted; component ready with typo ignored\n%s", d.run(t, "logs", "--since", since, f.Endpoint(c).Container))
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
			t.Fatal("unknown key did not produce a bounded startup rejection")
		})
	}
}
