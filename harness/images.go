package harness

import (
	"flag"
	"os"
	"strings"
)

type Component string

const (
	App        Component = "app"
	Executor   Component = "executor"
	CacheProxy Component = "cacheproxy"
	Release              = "enterprise-v2.314.0"
	RedisImage           = "mirror.gcr.io/library/redis:7.4.2-alpine@sha256:02419de7eddf55aa5bcf49efb74e88fa8d931b4d77c07eff8a6b2144472b6952"
)

// PinnedImages are released linux/amd64 images, never builds from source.
var PinnedImages = map[Component]string{
	App:        "gcr.io/flame-public/buildbuddy-app-enterprise@sha256:ed321abe352bc7fa58101e979ff13fd153ccef98a3609e44aef4075848629843",
	Executor:   "gcr.io/flame-public/buildbuddy-executor-enterprise@sha256:7d178c9ae49b0749dc0b509c471c1477ed548c8330441d37fed3b5a68ab364b8",
	CacheProxy: "gcr.io/flame-public/buildbuddy-proxy-enterprise@sha256:dcfd8fae2d63a5dc9b05e26d30e07817a638445d9ebac504963515fcfd47c906",
}
var repositories = map[Component]string{
	App:        "gcr.io/flame-public/buildbuddy-app-enterprise",
	Executor:   "gcr.io/flame-public/buildbuddy-executor-enterprise",
	CacheProxy: "gcr.io/flame-public/buildbuddy-proxy-enterprise",
}
var imageFlags = map[Component]*string{
	App:        flag.String("harness.app_image", "", "Released app image reference or version tag"),
	Executor:   flag.String("harness.executor_image", "", "Released executor image reference or version tag"),
	CacheProxy: flag.String("harness.cacheproxy_image", "", "Released cache proxy image reference or version tag"),
}

// Image precedence: Options.Images, flag, BB_<COMPONENT>_IMAGE, BB_VERSION,
// pinned digest. A short tag is expanded using the component's GCR repository.
func Image(c Component, overrides map[Component]string) string {
	v := overrides[c]
	if v == "" {
		v = *imageFlags[c]
	}
	if v == "" {
		v = os.Getenv("BB_" + strings.ToUpper(string(c)) + "_IMAGE")
	}
	if v == "" {
		v = os.Getenv("BB_VERSION")
	}
	if v == "" {
		return PinnedImages[c]
	}
	if strings.HasPrefix(v, "v") {
		v = "enterprise-" + v
	}
	if !strings.Contains(v, "/") {
		return repositories[c] + ":" + v
	}
	return v
}
