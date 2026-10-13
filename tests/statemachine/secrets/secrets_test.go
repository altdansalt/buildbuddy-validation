package secrets_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"net"
	"net/http"
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
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	delivered := make(chan string, 10)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	before, err := net.Interfaces()
	require.NoError(t, err)
	known := map[string]bool{}
	for _, iface := range before {
		known[iface.Name] = true
	}
	keydir := t.TempDir()
	require.NoError(t, os.Chmod(keydir, 0755))
	master := make([]byte, 32)
	_, err = rand.Read(master)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(keydir, "master"), master, 0644))
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.Executor},
		Config:     map[harness.Component]harness.Config{harness.App: {"app": harness.Config{"enable_secret_service": true}, "keystore": harness.Config{"master_key_uri": "local-insecure-kms://master", "local_insecure_kms_directory": "/fixture-kms"}}},
		DockerArgs: map[harness.Component][]string{harness.App: {"--volume", keydir + ":/fixture-kms:ro"}}})
	// The owned daemon has no default bridge, so host-gateway is unsuitable.
	// Discover the newly created fixture bridge's host address directly.
	after, err := net.Interfaces()
	require.NoError(t, err)
	gateways := []string{}
	for _, iface := range after {
		if known[iface.Name] || !strings.HasPrefix(iface.Name, "br-") {
			continue
		}
		addresses, err := iface.Addrs()
		require.NoError(t, err)
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			require.NoError(t, err)
			if ip.To4() != nil {
				gateways = append(gateways, ip.String())
			}
		}
	}
	require.Len(t, gateways, 1, "expected one new fixture-local bridge gateway")
	webhook := fmt.Sprintf("http://%s:%d", gateways[0], listener.Addr().(*net.TCPAddr).Port)
	localKey := f.Web.CreateAPIKey(t, f.GroupID, "secret-lifecycle-admin", []string{"ORG_ADMIN", "CACHE_WRITE", "SEND_NOTIFICATION"})
	c := harness.Dial(t, f.Endpoint(harness.App).GRPC, localKey)
	secondKey := f.Web.CreateAPIKey(t, f.GroupID, "second-secret-admin", []string{"ORG_ADMIN", "CACHE_WRITE", "SEND_NOTIFICATION"})
	second := harness.Dial(t, f.Endpoint(harness.App).GRPC, secondKey)
	active := c
	rpc := func(method string, req []byte) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var rsp []byte
		require.NoError(t, active.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &rsp, grpc.ForceCodec(wireCodec{})))
		return rsp
	}
	web := func(method string) map[string]any {
		rsp, err := f.Web.RPC(method, map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		require.NoError(t, err)
		return rsp
	}
	binaryWeb := func(method string, body ...[]byte) []byte {
		t.Helper()
		payload := field(1, field(2, []byte(f.GroupID)))
		for _, b := range body {
			payload = append(payload, b...)
		}
		frame := make([]byte, 5)
		binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
		frame = append(frame, payload...)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(frame))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/proto+prefixed")
		rsp, err := f.Web.Client.Do(req)
		require.NoError(t, err)
		defer rsp.Body.Close()
		data, err := io.ReadAll(rsp.Body)
		require.NoError(t, err)
		require.Equal(t, 200, rsp.StatusCode, "%s", data)
		status := rsp.Header.Get("grpc-status")
		var messages [][]byte
		for len(data) > 0 {
			require.GreaterOrEqual(t, len(data), 5)
			flag := data[0]
			size := int(binary.BigEndian.Uint32(data[1:5]))
			data = data[5:]
			require.GreaterOrEqual(t, len(data), size)
			if flag == 0x80 {
				for _, line := range strings.Split(string(data[:size]), "\r\n") {
					key, value, ok := strings.Cut(line, ":")
					if ok && strings.EqualFold(key, "grpc-status") {
						status = strings.TrimSpace(value)
					}
				}
			} else {
				require.Zero(t, flag)
				messages = append(messages, append([]byte(nil), data[:size]...))
			}
			data = data[size:]
		}
		require.Equal(t, "0", status)
		require.Len(t, messages, 1)
		return messages[0]
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
	consumer := func(filtered bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		root, err := c.UploadProto(ctx, "secrets", &repb.Directory{})
		require.NoError(t, err)
		// Presence markers distinguish unset variables from present empty values.
		script := ""
		expected := ""
		for _, n := range names {
			{
				script += fmt.Sprintf("printf '%%s ' \"${%s+x}\"; ", n)
				_, exists := model[n]
				if exists && (!filtered || n == "ALPHA_TOKEN") {
					expected += "x "
				} else {
					expected += " "
				}
			}
			script += fmt.Sprintf("printf '%%s' \"${%s-}\" | sha256sum | cut -d ' ' -f 1; ", n)
			value := model[n]
			if filtered && n != "ALPHA_TOKEN" {
				value = ""
			}
			h := sha256.Sum256([]byte(value))
			expected += hex.EncodeToString(h[:]) + "\n"
		}
		secretProperty := &repb.Platform_Property{Name: "include-secrets", Value: "true"}
		if filtered {
			secretProperty = &repb.Platform_Property{Name: "env-secrets", Value: "ALPHA_TOKEN"}
		}
		cmd, err := c.UploadProto(ctx, "secrets", &repb.Command{Arguments: []string{"/bin/sh", "-c", script}, Platform: &repb.Platform{Properties: []*repb.Platform_Property{{Name: "OSFamily", Value: "linux"}, {Name: "Arch", Value: "amd64"}, {Name: "workload-isolation-type", Value: "none"}, secretProperty}}})
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
		active = second
		require.Equal(t, string(b), string(rpc("ListSecrets", nil)), "second same-org API key")
		require.Equal(t, string(pubmsg), one(t, rpc("GetPublicKey", nil), 2))
		active = c
		binaryList := binaryWeb("ListSecrets")
		binaryNames := []string{}
		for _, secret := range fields(t, binaryList, 2) {
			binaryNames = append(binaryNames, one(t, secret, 1))
			require.Empty(t, one(t, secret, 2))
		}
		require.Equal(t, want, binaryNames)
		require.Equal(t, string(pubmsg), one(t, binaryWeb("GetPublicKey"), 2))
		rsp := web("ListSecrets")
		got = []string{}
		ss, ok := rsp["secret"].([]any)
		if value, present := rsp["secret"]; present {
			require.True(t, ok, "secret field is not an array: %v", value)
		} else {
			require.Empty(t, want, "nonempty model requires a secret field")
		}
		for key := range rsp {
			require.Contains(t, []string{"secret", "responseContext"}, key, "unexpected JSON listing field")
		}
		{
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
		consumer(false)
		consumer(true)
		// The notification consumer resolves the current secret on every request.
		notification := append(field(2, append(field(1, []byte("build-a")), field(1, []byte("build-b"))...)), field(3, field(2, field(1, []byte("ALPHA_TOKEN"))))...)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var response []byte
		err := c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/SendNotification", &notification, &response, grpc.ForceCodec(wireCodec{}))
		if value, exists := model["ALPHA_TOKEN"]; exists {
			require.NoError(t, err)
			select {
			case path := <-delivered:
				require.Equal(t, strings.TrimPrefix(value, webhook), path)
			case <-ctx.Done():
				t.Fatal("notification not delivered")
			}
		} else {
			require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
		}
		require.Equal(t, string(b), string(rpc("ListSecrets", nil)), "state remains stable after consumers")
	}
	check()
	check()
	for step, op := range []struct {
		name, value string
		del         bool
	}{{"ZETA_TOKEN", "first-value", false}, {"ALPHA_TOKEN", webhook + "/first", false}, {"ZETA_TOKEN", "replacement-value", false}, {"ZETA_TOKEN", "", false}, {"ZETA_TOKEN", "third-value", false}, {"ZETA_TOKEN", "third-value", false}, {"ALPHA_TOKEN", webhook + "/updated", false}, {"ZETA_TOKEN", "", true}, {"ZETA_TOKEN", "", true}, {"ZETA_TOKEN", "recreated-value", false}, {"ALPHA_TOKEN", "", true}, {"ZETA_TOKEN", "", true}} {
		t.Logf("transition name=%s delete=%v", op.name, op.del)
		secret := field(1, []byte(op.name))
		method := "DeleteSecret"
		if !op.del {
			sealed, err := box.SealAnonymous(nil, []byte(op.value), &pk, rand.Reader)
			require.NoError(t, err)
			secret = append(secret, field(2, []byte(base64.StdEncoding.EncodeToString(sealed)))...)
			method = "UpdateSecret"
		}
		started := time.Now()
		active = c
		if step%2 == 0 {
			active = second
		}
		if step%3 == 2 {
			binaryWeb(method, field(2, secret))
		} else {
			rpc(method, field(2, secret))
		}
		active = c
		if op.del {
			delete(model, op.name)

		} else {
			model[op.name] = op.value

		}
		check()
		observed := time.Since(started)
		check()
		t.Logf("first complete observation %s; second full observation stable", observed)
		t.Logf("all surfaces observed current stable state within %s (includes consumer execution)", time.Since(started))
	}
}
