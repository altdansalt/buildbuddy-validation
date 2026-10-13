package convergence_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Metadata's UI and gRPC interfaces use BuildBuddy-specific messages, which
// are not dependencies of this scaffold. Encode their released proto schema
// directly (cache.proto/resource.proto), preserving valid protobuf wire types.
func field(b []byte, n protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, n, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}
func metadata(ctx context.Context, c *harness.Clients, d *repb.Digest, cacheType uint64, method string) (map[int]uint64, error) {
	db, _ := proto.Marshal(d)
	rn := field(nil, 1, db)
	rn = protowire.AppendTag(rn, 4, protowire.VarintType)
	rn = protowire.AppendVarint(rn, cacheType)
	rn = protowire.AppendTag(rn, 5, protowire.VarintType)
	rn = protowire.AppendVarint(rn, 1)
	req := &emptypb.Empty{}
	req.ProtoReflect().SetUnknown(field(nil, 2, rn))
	resp := &emptypb.Empty{}
	if err := c.Conn.Invoke(ctx, method, req, resp); err != nil {
		return nil, err
	}
	result := map[int]uint64{}
	b := resp.ProtoReflect().GetUnknown()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			return nil, fmt.Errorf("metadata tag")
		}
		b = b[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				return nil, fmt.Errorf("metadata value")
			}
			result[int(n)] = v
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			if k < 0 {
				return nil, fmt.Errorf("metadata field")
			}
			b = b[k:]
		}
	}
	return result, nil
}

type model struct {
	result  *repb.ActionResult
	blobs   map[string][]byte
	digests []*repb.Digest
}

func TestReplacementConvergence(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	command, err := clients[0].UploadProto(ctx, "", &repb.Command{Arguments: []string{"/bin/sh", "-c", "printf result > result.txt"}, OutputPaths: []string{"result.txt"}})
	require.NoError(t, err)
	root, err := clients[0].UploadProto(ctx, "", &repb.Directory{})
	require.NoError(t, err)
	action, err := clients[0].UploadProto(ctx, "", &repb.Action{CommandDigest: command, InputRootDigest: root})
	require.NoError(t, err)
	for _, c := range clients {
		_, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: action, DigestFunction: repb.DigestFunction_SHA256})
		require.Equal(t, codes.NotFound, status.Code(err))
	}
	bb, err := exec.LookPath("bb")
	require.NoError(t, err, "runner must provide released bb CLI")
	var previousModify uint64
	for revision := 0; revision < 6; revision++ {
		writer := clients[revision%2]
		m := model{result: &repb.ActionResult{}, blobs: map[string][]byte{}}
		for i, label := range []string{"output", "stdout", "stderr"} {
			data := []byte(fmt.Sprintf("revision %d %s %s\n", revision, label, string(bytes.Repeat([]byte("x"), revision*41))))
			var d *repb.Digest
			if i%2 == 0 {
				d, err = writer.Upload(ctx, "", data)
			} else {
				d, err = writer.Write(ctx, "", data, false)
			}
			require.NoError(t, err)
			m.blobs[d.Hash] = data
			m.digests = append(m.digests, d)
		}
		m.result.OutputFiles = []*repb.OutputFile{{Path: "result.txt", Digest: m.digests[0], IsExecutable: revision%2 == 1}}
		if revision%2 == 0 {
			m.result.StdoutDigest = m.digests[1]
			m.result.StderrDigest = m.digests[2]
		} else {
			m.result.StdoutRaw = m.blobs[m.digests[1].Hash]
			m.result.StderrRaw = m.blobs[m.digests[2].Hash]
		}
		for repeat := 0; repeat < 2; repeat++ {
			updated, err := writer.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{ActionDigest: action, ActionResult: proto.Clone(m.result).(*repb.ActionResult), DigestFunction: repb.DigestFunction_SHA256})
			require.NoError(t, err)
			updated.ExecutionMetadata = nil
			require.True(t, proto.Equal(updated, m.result), "UpdateActionResult response differs from model")
			// The model changes only after a successful mutation. Poll all surfaces
			// together; once converged, further rounds must never return an old revision.
			deadline := time.Now().Add(15 * time.Second)
			var last error
			var first error
			for {
				pollCtx, pollCancel := context.WithDeadline(ctx, deadline)
				last = checkReads(pollCtx, clients, action, m)
				if last == nil {
					last = checkHTTP(pollCtx, f, action, m)
				}
				pollCancel()
				if first == nil && last != nil {
					first = last
					t.Logf("first convergence mismatch: %v", first)
				}
				if last == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("revision %d repeat %d: first=%v last=%v", revision, repeat, first, last)
				}
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					t.Fatal(ctx.Err())
				case <-timer.C:
				}
			}
			for stable := 0; stable < 3; stable++ {
				stableCtx, stableCancel := context.WithTimeout(ctx, 15*time.Second)
				err := checkReads(stableCtx, clients, action, m)
				if err == nil {
					err = checkHTTP(stableCtx, f, action, m)
				}
				stableCancel()
				require.NoError(t, err, "regression after convergence revision %d", revision)
			}
			for _, d := range append([]*repb.Digest{action}, m.digests...) {
				ct := uint64(2)
				if d == action {
					ct = 1
				}
				md, err := metadata(ctx, clients[0], d, ct, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata")
				require.NoError(t, err)
				require.Greater(t, md[2], uint64(0))
				require.Greater(t, md[4], uint64(0))
				if ct == 1 {
					require.Greater(t, md[4], previousModify, "AC metadata mtime must reflect completed write")
					previousModify = md[4]
				}
				ui, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "cacheType": ct, "digestFunction": 1}})
				require.NoError(t, err)
				require.Equal(t, strconv.FormatUint(md[2], 10), fmt.Sprint(ui["storedSizeBytes"]))
				if ct == 2 {
					require.Equal(t, uint64(d.SizeBytes), md[5])
					for _, c := range clients {
						direct, err := metadata(ctx, c, d, ct, "/cache.service.Cache/GetMetadata")
						require.NoError(t, err)
						require.Equal(t, uint64(d.SizeBytes), direct[5])
						require.Greater(t, direct[2], uint64(0))
					}
				}
			}
			for i := range clients {
				for _, d := range m.digests {
					cmd := exec.CommandContext(ctx, bb, "download", "--target=grpc://"+f.Endpoint([]harness.Component{harness.App, harness.CacheProxy}[i]).GRPC, "--api_key="+f.APIKey, d.Hash+"/"+strconv.FormatInt(d.SizeBytes, 10))
					cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
					var stderr bytes.Buffer
					cmd.Stderr = &stderr
					out, err := cmd.Output()
					require.NoError(t, err, "bb download: %s", stderr.String())
					require.Equal(t, m.blobs[d.Hash], out)
				}
			}
		}
	}
}
func checkReads(ctx context.Context, clients []*harness.Clients, action *repb.Digest, m model) error {
	for side, c := range clients {
		for mask := 0; mask < 8; mask++ {
			inline := mask&4 != 0
			req := &repb.GetActionResultRequest{ActionDigest: action, DigestFunction: repb.DigestFunction_SHA256, InlineStdout: mask&1 != 0, InlineStderr: mask&2 != 0}
			if inline {
				req.InlineOutputFiles = []string{"result.txt"}
			}
			got, err := c.AC.GetActionResult(ctx, req)
			if err != nil {
				return fmt.Errorf("side %d: %w", side, err)
			}
			expected := proto.Clone(m.result).(*repb.ActionResult)
			// Server-assigned execution provenance is outside the mutable model.
			got.ExecutionMetadata = nil
			if inline {
				expected.OutputFiles[0].Contents = m.blobs[m.digests[0].Hash]
			}
			if !proto.Equal(got, expected) {
				return fmt.Errorf("side %d inline %v got %v expected %v", side, inline, got, expected)
			}
		}
		missing, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{BlobDigests: m.digests, DigestFunction: repb.DigestFunction_SHA256})
		if err != nil {
			return err
		}
		if len(missing.MissingBlobDigests) > 0 {
			return fmt.Errorf("side %d missing blobs", side)
		}
		batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: m.digests, DigestFunction: repb.DigestFunction_SHA256})
		if err != nil {
			return err
		}
		if len(batch.Responses) != len(m.digests) {
			return fmt.Errorf("short batch")
		}
		for _, r := range batch.Responses {
			if r.GetStatus().GetCode() != 0 || !bytes.Equal(r.Data, m.blobs[r.Digest.Hash]) {
				return fmt.Errorf("side %d wrong batch output", side)
			}
		}
		for _, d := range m.digests {
			data, err := c.Read(ctx, "", d)
			if err != nil {
				return err
			}
			if !bytes.Equal(data, m.blobs[d.Hash]) {
				return fmt.Errorf("side %d wrong ByteStream output", side)
			}
		}
	}
	return nil
}

// The action details page obtains AC protos and output bytes via this HTTP
// file endpoint, not via the JSON metadata endpoint alone.
func checkHTTP(ctx context.Context, f *harness.Fixture, action *repb.Digest, m model) error {
	for _, mode := range []string{"view", "download"} {
		for _, d := range append([]*repb.Digest{action}, m.digests...) {
			scheme := "bytestream"
			if d == action {
				scheme = "actioncache"
			}
			name := harness.ResourceName("", d, false)
			if d == action {
				name = "blobs/ac/" + d.Hash + "/" + strconv.FormatInt(d.SizeBytes, 10)
			}
			resource := scheme + "://app:1985/" + name
			req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+mode+"?bytestream_url="+url.QueryEscape(resource)+"&request_context="+url.QueryEscape(base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID)))), nil)
			if err != nil {
				return err
			}
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			resp, err := f.Web.Client.Do(req)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %s %s: %d %s", mode, scheme, resp.StatusCode, data)
			}
			if d == action {
				got := &repb.ActionResult{}
				if err := proto.Unmarshal(data, got); err != nil {
					return err
				}
				got.ExecutionMetadata = nil
				if !proto.Equal(got, m.result) {
					return fmt.Errorf("HTTP %s stale action result: %v", mode, got)
				}
			} else if !bytes.Equal(data, m.blobs[d.Hash]) {
				return fmt.Errorf("HTTP %s wrong output bytes", mode)
			}
		}
	}
	return nil
}
