package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// ChromiumImage pins the headless-shell OCI index (linux/amd64 is selected).
const ChromiumImage = "mirror.gcr.io/chromedp/headless-shell@sha256:f70908c8020555a3bf89a070140913b443a03ead28c1a66563c3912a3b32b08d"

// Browser is a single authenticated Chromium tab. Its lifetime is bounded by ctx.
// Use separate browsers for separate sessions; SelfAuth provides only one identity.
type Browser struct {
	Context context.Context
	origin  string
}

// Browser starts Chromium beside the app and imports a fixture-local user's real
// login cookies. group must be an organization that user can access.
func (f *Fixture) Browser(ctx context.Context, session *WebSession, group string) *Browser {
	f.t.Helper()
	if session == nil || session.Client.Jar == nil {
		f.t.Fatal("browser requires a cookie-authenticated WebSession")
	}
	origin, err := url.Parse(session.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	if session.URL != f.Endpoint(App).SelfAuthURL {
		f.t.Fatal("browser session must belong to this fixture")
	}
	name := fmt.Sprintf("%s-browser-%d", f.ID, len(f.containers))
	f.containers = append(f.containers, name)
	// The image forwards 9222 to Chromium on 9223; preserve its entrypoint flags.
	f.docker("run", "-d", "--name", name, "--network", f.Network, "--platform", "linux/amd64", "--shm-size=256m", "-p", "127.0.0.1::9222", ChromiumImage)
	endpoint := "http://" + f.port(name, "9222")
	client := &http.Client{Timeout: 2 * time.Second}
	var ws string
	var lastErr error
	for ws == "" {
		req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/json/version", nil)
		if err != nil {
			f.t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			var v struct {
				WebSocket string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			lastErr = err
			if err == nil && v.WebSocket != "" {
				u, e := url.Parse(v.WebSocket)
				if e != nil {
					f.t.Fatal(e)
				}
				published, _ := url.Parse(endpoint)
				u.Host = published.Host
				ws = u.String()
			}
		}
		if err != nil {
			lastErr = err
		}
		if ws != "" {
			break
		}
		select {
		case <-ctx.Done():
			logs, _ := f.command("logs", name)
			f.t.Fatalf("Chromium readiness: %v (last error: %v)\n%s", ctx.Err(), lastErr, logs)
		case <-time.After(100 * time.Millisecond):
		}
	}
	alloc, cancelAlloc := chromedp.NewRemoteAllocator(ctx, ws, chromedp.NoModifyURL)
	tab, cancelTab := chromedp.NewContext(alloc)
	b := &Browser{Context: tab, origin: strings.TrimRight(session.URL, "/")}
	f.t.Cleanup(func() {
		defer cancelAlloc()
		defer cancelTab()
		if f.t.Failed() {
			capture, cancel := context.WithTimeout(tab, 5*time.Second)
			defer cancel()
			var png []byte
			if chromedp.Run(capture, chromedp.FullScreenshot(&png, 80)) == nil {
				_ = os.WriteFile(filepath.Join(f.OutputDir, name+".png"), png, 0600)
			}
			var text string
			if chromedp.Run(capture, chromedp.Text("body", &text, chromedp.ByQuery)) == nil {
				_ = os.WriteFile(filepath.Join(f.OutputDir, name+".body.txt"), []byte(text), 0600)
			}
		}
	})
	err = chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
		for _, c := range session.Client.Jar.Cookies(origin) {
			if err := network.SetCookie(c.Name, c.Value).WithURL(session.URL).Do(ctx); err != nil {
				return err
			}
		}
		return network.SetCookie("Selected-Group-ID", group).WithURL(session.URL).Do(ctx)
	}))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// Open navigates to a fixture-local path and waits for a visible CSS selector.
// Pick a page-specific selector that appears only after its data has loaded.
func (b *Browser) Open(path, readySelector string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return fmt.Errorf("expected fixture-local absolute path")
	}
	return chromedp.Run(b.Context, chromedp.Navigate(b.origin+path), chromedp.WaitVisible(readySelector, chromedp.ByQuery))
}

// Text returns rendered text from an element matched by a CSS selector.
func (b *Browser) Text(selector string) (string, error) {
	var text string
	err := chromedp.Run(b.Context, chromedp.Text(selector, &text, chromedp.ByQuery))
	return text, err
}

// Run exposes chromedp actions for element queries, clicks and further waits.
func (b *Browser) Run(actions ...chromedp.Action) error { return chromedp.Run(b.Context, actions...) }
