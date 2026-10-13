package streams

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The file API's uri/data messages have the same wire fields as these wrappers.
// Unknown fields supply GetFileRange's int64 start=2 and end=3 without adding
// a dependency on unreleased BuildBuddy source or generated API bindings.
func rangeRequest(uri string, start, end int64) *wrapperspb.StringValue {
	r := wrapperspb.String(uri)
	b := protowire.AppendTag(nil, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(start))
	b = protowire.AppendTag(b, 3, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(end))
	r.ProtoReflect().SetUnknown(b)
	return r
}
func read(ctx context.Context, c *harness.Clients, name string, offset, limit int64) ([]byte, error) {
	s, e := c.ByteStream.Read(ctx, &bspb.ReadRequest{ResourceName: name, ReadOffset: offset, ReadLimit: limit})
	if e != nil {
		return nil, e
	}
	var b []byte
	for {
		r, e := s.Recv()
		if e == io.EOF {
			return b, nil
		}
		if e != nil {
			return nil, e
		}
		b = append(b, r.Data...)
	}
}
func file(ctx context.Context, c *harness.Clients, uri string) ([]byte, error) {
	s, e := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile")
	if e != nil {
		return nil, e
	}
	if e = s.SendMsg(wrapperspb.String(uri)); e != nil {
		return nil, e
	}
	if e = s.CloseSend(); e != nil {
		return nil, e
	}
	var b []byte
	for {
		r := new(wrapperspb.BytesValue)
		e = s.RecvMsg(r)
		if e == io.EOF {
			return b, nil
		}
		if e != nil {
			return nil, e
		}
		b = append(b, r.Value...)
	}
}
func rest(ctx context.Context, f *harness.Fixture, method string, req any) ([]byte, error) {
	b, e := json.Marshal(req)
	if e != nil {
		return nil, e
	}
	r, e := http.NewRequestWithContext(ctx, "POST", f.Endpoint(harness.App).HTTP+"/api/v1/"+method, bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, e := http.DefaultClient.Do(r)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	b, e = io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s HTTP %d: %s", method, res.StatusCode, b)
	}
	return b, e
}
func equal(surface string, got, want []byte, e error) error {
	if e != nil {
		return fmt.Errorf("%s: %w", surface, e)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s: bytes differ (got %d want %d)", surface, len(got), len(want))
	}
	return nil
}
func check(ctx context.Context, f *harness.Fixture, clients []*harness.Clients, instance string, d *repb.Digest, want []byte) error {
	for i, c := range clients {
		_, statusErr := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: harness.UploadResourceName(instance, d, false)})
		if status.Code(statusErr) != codes.Unimplemented {
			return fmt.Errorf("endpoint %d QueryWriteStatus contract changed: %v", i, statusErr)
		}
		for _, compressed := range []bool{false, true} {
			name := harness.ResourceName(instance, d, compressed)
			for _, r := range [][2]int64{{0, 0}, {0, min(int64(len(want)), 311)}, {max(0, int64(len(want)-101)), min(int64(len(want)), 101)}} {
				b, e := read(ctx, c, name, r[0], r[1])
				if e == nil && compressed {
					b, e = harness.Decompress(b)
				}
				expected := want[r[0]:]
				if r[1] > 0 {
					expected = expected[:r[1]]
				}
				if e = equal(fmt.Sprintf("endpoint %d BS zstd=%t range=%v", i, compressed, r), b, expected, e); e != nil {
					return e
				}
			}
		}
		for _, compressors := range [][]repb.Compressor_Value{nil, {repb.Compressor_ZSTD}} {
			res, e := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}, AcceptableCompressors: compressors, DigestFunction: repb.DigestFunction_SHA256})
			if e != nil {
				return e
			}
			if len(res.Responses) != 1 {
				return fmt.Errorf("batch response count")
			}
			r := res.Responses[0]
			if r.GetStatus().GetCode() != 0 {
				return fmt.Errorf("batch status: %v", r.Status)
			}
			if r.Digest.Hash != d.Hash || r.Digest.SizeBytes != d.SizeBytes {
				return fmt.Errorf("batch digest mismatch")
			}
			b := r.Data
			if r.Compressor == repb.Compressor_ZSTD {
				b, e = harness.Decompress(b)
			}
			if e = equal("BatchRead", b, want, e); e != nil {
				return e
			}
		}
		missing, e := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		if e != nil {
			return e
		}
		if len(missing.MissingBlobDigests) != 0 {
			return fmt.Errorf("endpoint %d missing blob", i)
		}
	}
	app := clients[0]
	for _, compressed := range []bool{false, true} {
		uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, compressed)
		b, e := file(ctx, app, uri)
		if e == nil && compressed {
			b, e = harness.Decompress(b)
		}
		if e = equal("gRPC GetFile", b, want, e); e != nil {
			return e
		}
		for _, handler := range []string{"download", "view"} {
			req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+handler+"?bytestream_url="+url.QueryEscape(uri)+"&filename=stream.txt", nil)
			if err != nil {
				return err
			}
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			res, err := f.Web.Client.Do(req)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				return fmt.Errorf("UI %s HTTP %d: %s", handler, res.StatusCode, data)
			}
			if compressed && err == nil {
				data, err = harness.Decompress(data)
			}
			if err = equal("UI "+handler, data, want, err); err != nil {
				return err
			}
		}

		b, e = rest(ctx, f, "GetFile", map[string]any{"uri": uri})
		if e == nil && compressed {
			b, e = harness.Decompress(b)
		}
		if e = equal("REST GetFile", b, want, e); e != nil {
			return e
		}
	}
	uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, false)
	for _, r := range [][2]int64{{0, min(int64(len(want)), 137)}, {min(17, int64(len(want)-1)), min(int64(len(want)), 328)}, {max(0, int64(len(want)-101)), int64(len(want))}} {
		response := new(wrapperspb.BytesValue)
		e := app.Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", rangeRequest(uri, r[0], r[1]), response)
		if e = equal("gRPC range", response.Value, want[r[0]:r[1]], e); e != nil {
			return e
		}
		b, e := rest(ctx, f, "GetFileRange", map[string]any{"uri": uri, "start": r[0], "end": r[1]})
		if e != nil {
			return e
		}
		var result struct{ Data string }
		if e = json.Unmarshal(b, &result); e != nil {
			return e
		}
		b, e = base64.StdEncoding.DecodeString(result.Data)
		if e = equal("REST range", b, want[r[0]:r[1]], e); e != nil {
			return e
		}
	}
	m, e := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
	if e != nil {
		return e
	}
	if fmt.Sprint(m["digestSizeBytes"]) != fmt.Sprint(d.SizeBytes) {
		return fmt.Errorf("UI metadata logical size: %v", m)
	}

	vals := map[int]int64{}
	for field, key := range map[int]string{2: "storedSizeBytes", 3: "lastAccessUsec", 4: "lastModifyUsec", 5: "digestSizeBytes"} {
		n, err := strconv.ParseInt(fmt.Sprint(m[key]), 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("UI metadata %s: %v", key, m)
		}
		vals[field] = n
	}
	if vals[3] > time.Now().Add(time.Minute).UnixMicro() || vals[4] > time.Now().Add(time.Minute).UnixMicro() {
		return fmt.Errorf("metadata bounds: %v", m)
	}
	resource := fieldBytes(nil, 1, mustMarshal(d))
	resource = fieldBytes(resource, 2, []byte(instance))
	resource = fieldInt(resource, 4, 2)
	resource = fieldInt(resource, 5, 1)
	req := wire(fieldBytes(nil, 2, resource))
	for _, method := range []string{"/buildbuddy.service.BuildBuddyService/GetCacheMetadata", "/cache.service.Cache/GetMetadata"} {
		response := new(emptypb.Empty)
		if err := app.Conn.Invoke(ctx, method, req, response); err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		fields := ints(response.ProtoReflect().GetUnknown())
		if fields[5] != d.SizeBytes || fields[2] != vals[2] || fields[3] <= 0 || fields[4] != vals[4] {
			return fmt.Errorf("%s metadata disagrees: %v UI=%v", method, fields, vals)
		}
	}

	frames, err := uiStream(ctx, f, "GetCacheMetadata", req.ProtoReflect().GetUnknown())
	if err != nil {
		return err
	}
	if len(frames) != 1 {
		return fmt.Errorf("UI framed metadata response count %d", len(frames))
	}
	framed := ints(frames[0])
	if framed[5] != d.SizeBytes || framed[2] != vals[2] || framed[4] != vals[4] || framed[3] < vals[3] {
		return fmt.Errorf("UI framed metadata disagrees: %v", framed)
	}

	response := new(emptypb.Empty)
	if err := clients[1].Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", req, response); err != nil {
		return fmt.Errorf("proxy metadata: %w", err)
	}
	fields := ints(response.ProtoReflect().GetUnknown())
	if fields[5] != d.SizeBytes || fields[2] <= 0 || fields[3] <= 0 || fields[4] <= 0 {
		return fmt.Errorf("proxy metadata invalid: %v", fields)
	}

	binary, e := exec.LookPath("bb")
	if e != nil {
		return fmt.Errorf("required bb download surface unavailable: %w", e)
	}
	dir, e := os.MkdirTemp("", "stream-download-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
		for _, compressed := range []bool{false, true} {
			out := filepath.Join(dir, string(component))
			cmd := exec.CommandContext(ctx, binary, "download", harness.ResourceName(instance, d, compressed), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+out)
			logs, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("bb download via %s: %v %s", component, err, logs)
			}
			b, err := os.ReadFile(out)
			if err = equal("bb download", b, want, err); err != nil {
				return err
			}
		}
	}
	return nil
}

func fieldBytes(b []byte, n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v)
}
func fieldInt(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func wire(b []byte) *emptypb.Empty { r := new(emptypb.Empty); r.ProtoReflect().SetUnknown(b); return r }
func mustMarshal(m proto.Message) []byte {
	b, e := proto.Marshal(m)
	if e != nil {
		panic(e)
	}
	return b
}
func ints(b []byte) map[int]int64 {
	out := map[int]int64{}
	for len(b) > 0 {
		n, t, k := protowire.ConsumeTag(b)
		if k < 0 {
			break
		}
		b = b[k:]
		if t == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				break
			}
			out[int(n)] = int64(v)
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, t, b)
			if k < 0 {
				break
			}
			b = b[k:]
		}
	}
	return out
}

// UI RPC transport: exactly the length-prefixed protobuf protocol used by rpc_service.ts.
func uiStream(ctx context.Context, f *harness.Fixture, method string, request []byte) ([][]byte, error) {
	frame := make([]byte, 5)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(request)))
	frame = append(frame, request...)
	req, err := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/proto+prefixed")
	req.Header.Set("x-buildbuddy-api-key", f.APIKey)
	res, err := f.Web.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("UI stream HTTP %d", res.StatusCode)
	}
	var messages [][]byte
	statusOK := res.Header.Get("grpc-status") == "0"
	for {
		header := make([]byte, 5)
		_, err = io.ReadFull(res.Body, header)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(header[1:])
		if n > 16<<20 {
			return nil, fmt.Errorf("oversized UI frame")
		}
		body := make([]byte, n)
		if _, err = io.ReadFull(res.Body, body); err != nil {
			return nil, err
		}
		if header[0] == 0x80 {
			if !strings.Contains(strings.ToLower(string(body)), "grpc-status: 0\r\n") {
				return nil, fmt.Errorf("UI stream status: %s", body)
			}
			statusOK = true
		} else if header[0] == 0 {
			messages = append(messages, body)
		} else {
			return nil, fmt.Errorf("unsupported frame flags %d", header[0])
		}
	}
	if !statusOK {
		return nil, fmt.Errorf("missing UI stream success status")
	}
	return messages, nil
}
func byteFields(b []byte, field protowire.Number) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		n, t, k := protowire.ConsumeTag(b)
		if k < 0 {
			break
		}
		b = b[k:]
		if t == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				break
			}
			if n == field {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, t, b)
			if k < 0 {
				break
			}
			b = b[k:]
		}
	}
	return out
}

// Check the missing-state model without stopping at the first disagreeing surface.
func absent(ctx context.Context, f *harness.Fixture, clients []*harness.Clients, instance string, d *repb.Digest) []string {
	var issues []string
	record := func(surface string, err error) {
		if status.Code(err) != codes.NotFound {
			issues = append(issues, fmt.Sprintf("%s expected NotFound: %v", surface, err))
		}
	}
	resource := fieldBytes(nil, 1, mustMarshal(d))
	resource = fieldBytes(resource, 2, []byte(instance))
	resource = fieldInt(resource, 4, 2)
	resource = fieldInt(resource, 5, 1)
	request := wire(fieldBytes(nil, 2, resource))
	for i, c := range clients {
		for _, compressed := range []bool{false, true} {
			name := harness.ResourceName(instance, d, compressed)
			_, err := read(ctx, c, name, 0, 0)
			record(fmt.Sprintf("endpoint%d ByteStream zstd=%t", i, compressed), err)
		}
		r, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: instance, BlobDigests: []*repb.Digest{d}})
		if err != nil || len(r.GetMissingBlobDigests()) != 1 || !proto.Equal(r.GetMissingBlobDigests()[0], d) {
			issues = append(issues, fmt.Sprintf("endpoint%d FindMissing retained digest: %v %v", i, r, err))
		}
		batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: instance, Digests: []*repb.Digest{d}})
		if err != nil || len(batch.GetResponses()) != 1 || batch.Responses[0].GetStatus().GetCode() != int32(codes.NotFound) || !proto.Equal(batch.Responses[0].Digest, d) {
			issues = append(issues, fmt.Sprintf("endpoint%d BatchRead retained data/status", i))
		}
		record(fmt.Sprintf("endpoint%d metadata", i), c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", request, new(emptypb.Empty)))
		binary, err := exec.LookPath("bb")
		if err != nil {
			issues = append(issues, "bb missing")
		} else {
			cmd := exec.CommandContext(ctx, binary, "download", harness.ResourceName(instance, d, false), "--target=grpc://"+f.Endpoint([]harness.Component{harness.App, harness.CacheProxy}[i]).GRPC, "--api_key="+f.APIKey)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "NotFound") {
				issues = append(issues, fmt.Sprintf("endpoint%d CLI missing-state disagrees: %v", i, err))
			}
		}
	}
	record("app UI gRPC metadata", clients[0].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", request, new(emptypb.Empty)))
	_, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": fmt.Sprint(d.SizeBytes)}, "instanceName": instance, "cacheType": 2, "digestFunction": 1}})
	if err == nil || !strings.Contains(err.Error(), "NotFound") {
		issues = append(issues, fmt.Sprintf("UI metadata missing-state: %v", err))
	}
	for _, compressed := range []bool{false, true} {
		uri := "bytestream://app:1985/" + harness.ResourceName(instance, d, compressed)
		_, err := file(ctx, clients[0], uri)
		if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "failed to read byte stream resource") {
			issues = append(issues, fmt.Sprintf("app GetFile missing-state: %v", err))
		}
		// Compressed ranges have their own contract defect; missing blobs should still be missing.
		record("app GetFileRange", clients[0].Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", rangeRequest(uri, 0, 1), new(wrapperspb.BytesValue)))
		for _, method := range []string{"GetFile", "GetFileRange"} {
			request := map[string]any{"uri": uri}
			if method == "GetFileRange" {
				request["start"] = 0
				request["end"] = 1
			}
			_, err := rest(ctx, f, method, request)
			if err == nil || !(strings.Contains(err.Error(), "NotFound") || (method == "GetFile" && strings.Contains(err.Error(), "GetFile HTTP 404:") && strings.Contains(err.Error(), "failed to read byte stream resource"))) {
				issues = append(issues, fmt.Sprintf("REST %s missing-state: %v", method, err))
			}
		}
		for _, handler := range []string{"download", "view"} {
			req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+"/file/"+handler+"?bytestream_url="+url.QueryEscape(uri)+"&filename=stream.txt", nil)
			if err != nil {
				issues = append(issues, err.Error())
				continue
			}
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			res, err := f.Web.Client.Do(req)
			if err != nil {
				issues = append(issues, err.Error())
				continue
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || res.StatusCode == 200 || !(strings.Contains(string(body), "NotFound") || (res.StatusCode == 400 && strings.Contains(string(body), "Missing invocation_id param"))) {
				issues = append(issues, fmt.Sprintf("UI %s missing-state: HTTP %d", handler, res.StatusCode))
			}
		}
	}
	return issues
}
