package secrets_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/nacl/box"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Wire fields below match released proto/secrets.proto. Using a protobuf codec
// avoids importing (or building) the product source into the validation repo.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func field(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func fields(t *testing.T, b []byte, n protowire.Number) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		num, typ, k := protowire.ConsumeTag(b)
		require.Greater(t, k, 0)
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			require.GreaterOrEqual(t, k, 0)
			if num == n {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(num, typ, b)
			require.GreaterOrEqual(t, k, 0)
			b = b[k:]
		}
	}
	return out
}
func one(t *testing.T, b []byte, n protowire.Number) string {
	v := fields(t, b, n)
	if len(v) == 0 {
		return ""
	}
	require.Len(t, v, 1)
	return string(v[0])
}

func TestEncryptedLifecycle(t *testing.T) {
	keydir := t.TempDir()
	require.NoError(t, os.Chmod(keydir, 0755))
	master := make([]byte, 32)
	_, err := rand.Read(master)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(keydir, "master"), master, 0644))
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor},
		Config:     map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"enable_secret_service": true}, "keystore": harness.Config{"master_key_uri": "local-insecure-kms://master", "local_insecure_kms_directory": "/fixture-kms"}}},
		DockerArgs: map[harness.Component][]string{harness.App: {"--volume", keydir + ":/fixture-kms:ro"}}})
	localKey := f.Web.CreateAPIKey(t, f.GroupID, "secret-lifecycle-admin", []string{"ORG_ADMIN", "CACHE_WRITE"})
	c := harness.Dial(t, f.Endpoint(harness.App).GRPC, localKey)
	rpc := func(method string, req []byte) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var rsp []byte
		require.NoError(t, c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &rsp, grpc.ForceCodec(wireCodec{})))
		return rsp
	}
	web := func(method string) map[string]any {
		rsp, err := f.Web.RPC(method, map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		require.NoError(t, err)
		return rsp
	}
	pubmsg := []byte(one(t, rpc("GetPublicKey", nil), 2))
	pub := one(t, pubmsg, 2)
	id := one(t, pubmsg, 1)
	require.NotEmpty(t, id)
	raw, err := base64.StdEncoding.DecodeString(pub)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	var pk [32]byte
	copy(pk[:], raw)
	model := map[string]string{}
	names := []string{"ALPHA_TOKEN", "ZETA_TOKEN"}
	consumer := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		root, err := c.UploadProto(ctx, "secrets", &repb.Directory{})
		require.NoError(t, err)
		// Only hashes leave the consumer. Empty string models an absent variable.
		script := ""
		expected := ""
		for _, n := range names {
			script += fmt.Sprintf("printf '%%s' \"${%s-}\" | sha256sum | cut -d ' ' -f 1; ", n)
			h := sha256.Sum256([]byte(model[n]))
			expected += hex.EncodeToString(h[:]) + "\n"
		}
		cmd, err := c.UploadProto(ctx, "secrets", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}, {Name: "include-secrets", Value: "true"}}}})
		require.NoError(t, err)
		action, err := c.UploadProto(ctx, "secrets", &repb.Action{CommandDigest: cmd, InputRootDigest: root, Timeout: durationpb.New(30 * time.Second), DoNotCache: true})
		require.NoError(t, err)
		stream, err := c.Execution.Execute(ctx, &repb.ExecuteRequest{InstanceName: "secrets", ActionDigest: action, SkipCacheLookup: true})
		require.NoError(t, err)
		for {
			op, err := stream.Recv()
			require.NotEqual(t, io.EOF, err, "no completed operation")
			require.NoError(t, err)
			if !op.Done {
				continue
			}
			require.Nil(t, op.GetError())
			rsp := &repb.ExecuteResponse{}
			require.NoError(t, op.GetResponse().UnmarshalTo(rsp))
			require.Zero(t, rsp.GetStatus().GetCode(), "%v", rsp.GetStatus())
			require.Zero(t, rsp.GetResult().GetExitCode())
			out := rsp.GetResult().GetStdoutRaw()
			if d := rsp.GetResult().GetStdoutDigest(); d != nil {
				out, err = c.Read(ctx, "secrets", d)
				require.NoError(t, err)
			}
			require.Equal(t, expected, string(out))
			break
		}
	}
	cli, cliErr := exec.LookPath("bb")
	require.NoError(t, cliErr, "bb secrets list requires a runner-installed bb binary")
	check := func() {
		want := make([]string, 0, len(model))
		for n := range model {
			want = append(want, n)
		}
		sort.Strings(want)
		b := rpc("ListSecrets", nil)
		got := []string{}
		for _, s := range fields(t, b, 2) {
			got = append(got, one(t, s, 1))
			require.Empty(t, one(t, s, 2), "list exposed secret value")
		}
		require.Equal(t, want, got)
		rsp := web("ListSecrets")
		got = []string{}
		if ss, ok := rsp["secret"].([]any); ok {
			for _, v := range ss {
				s := v.(map[string]any)
				got = append(got, s["name"].(string))
				require.True(t, s["value"] == nil || s["value"] == "", "HTTP list exposed value")
			}
		}
		require.Equal(t, want, got)
		require.Equal(t, string(pubmsg), one(t, rpc("GetPublicKey", nil), 2))
		wp := web("GetPublicKey")["publicKey"].(map[string]any)
		require.Equal(t, pub, wp["value"])
		require.Equal(t, id, wp["id"])
		{
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, cli, "secrets", "--target=grpc://"+f.Endpoint(harness.App).GRPC, "list")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "BUILDBUDDY_API_KEY=" + localKey}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			require.Equal(t, strings.Join(want, "\n"), strings.TrimSpace(string(out)))
		}
		consumer()
	}
	check()
	for _, op := range []struct {
		name, value string
		del         bool
	}{{"ZETA_TOKEN", "first-value", false}, {"ALPHA_TOKEN", "independent-value", false}, {"ZETA_TOKEN", "replacement-value", false}, {"ZETA_TOKEN", "replacement-value", false}, {"ZETA_TOKEN", "", true}, {"ZETA_TOKEN", "", true}, {"ZETA_TOKEN", "recreated-value", false}, {"ALPHA_TOKEN", "", true}, {"ZETA_TOKEN", "", true}} {
		t.Logf("transition name=%s delete=%v", op.name, op.del)
		secret := field(1, []byte(op.name))
		method := "DeleteSecret"
		if !op.del {
			sealed, err := box.SealAnonymous(nil, []byte(op.value), &pk, rand.Reader)
			require.NoError(t, err)
			secret = append(secret, field(2, []byte(base64.StdEncoding.EncodeToString(sealed)))...)
			method = "UpdateSecret"
		}
		rpc(method, field(2, secret))
		if op.del {
			delete(model, op.name)
		} else {
			model[op.name] = op.value
		}
		check()
	}
}
