package isolation_test

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPrivateImageCacheRejectsWrongCredentials(t *testing.T) {
	f := fixture(t)
	registry, valid := localRegistry(t, f)
	config := valid["test-registry-config"]
	delete(valid, "test-registry-config")
	docker(t, "pull", "mirror.gcr.io/library/alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c")
	image := registry + "/private:workload"
	docker(t, "tag", "mirror.gcr.io/library/alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c", image)
	docker(t, "--config", config, "push", image)
	t.Logf("private workload digest: %s", docker(t, "image", "inspect", image, "--format", "{{index .RepoDigests 0}}"))
	docker(t, "image", "rm", image)
	wrong := map[string]string{"container-registry-username": "fixture", "container-registry-password": "wrong"}
	cold := execute(t, f, image, "echo private-workload", "", wrong)
	require.NotZero(t, cold.GetStatus().GetCode(), "wrong credentials must fail before image caching")
	require.Empty(t, stdout(t, f, cold))
	warm := execute(t, f, image, "echo private-workload", "", valid)
	success(t, warm)
	require.Equal(t, "private-workload\n", stdout(t, f, warm))
	cached := execute(t, f, image, "echo private-workload", "", wrong)
	t.Logf("wrong credentials while cached: status=%v stdout=%q", cached.GetStatus(), stdout(t, f, cached))
	// Run the eviction control before the failing assertion, so the invocation
	// shows the same credentials are rejected by the real registry when cold.
	docker(t, "image", "rm", image)
	evicted := execute(t, f, image, "echo private-workload", "", wrong)
	require.NotZero(t, evicted.GetStatus().GetCode(), "wrong credentials must fail after explicit eviction")
	require.Empty(t, stdout(t, f, evicted))
	require.NotZero(t, cached.GetStatus().GetCode(), "cached private image must not authorize different registry credentials")
	require.Empty(t, stdout(t, f, cached))
}
