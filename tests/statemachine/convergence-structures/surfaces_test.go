package structures_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func number(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func scalar(b []byte, target protowire.Number) (uint64, error) {
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			return 0, fmt.Errorf("invalid tag")
		}
		b = b[k:]
		k = protowire.ConsumeFieldValue(n, typ, b)
		if k < 0 {
			return 0, fmt.Errorf("invalid field")
		}
		if n == target {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				return 0, fmt.Errorf("invalid integer")
			}
			return v, nil
		}
		b = b[k:]
	}
	return 0, nil
}
func streamBytes(ctx context.Context, c *harness.Clients, method string, req []byte) ([]byte, error) {
	s, err := c.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, method, grpc.ForceCodec(wireCodec{}))
	if err != nil {
		return nil, err
	}
	if err = s.SendMsg(req); err != nil {
		return nil, err
	}
	if err = s.CloseSend(); err != nil {
		return nil, err
	}
	var out []byte
	for {
		var b []byte
		err = s.RecvMsg(&b)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		dataField := protowire.Number(1)
		if method == "/google.bytestream.ByteStream/Read" {
			dataField = 10
		}
		fs, err := fields(b, dataField)
		if err != nil {
			return nil, err
		}
		for _, v := range fs {
			out = append(out, v...)
		}
	}
}
func (m *model) extraSurfaces(ctx context.Context, f *harness.Fixture, cs []*harness.Clients) error {
	keys := make([]string, 0, len(m.blobs))
	for k := range m.blobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := m.blobs[k]
		_, manifestOnly := m.mappings[k]
		d := harness.Digest(want)
		dg, _ := proto.Marshal(d)
		for _, c := range cs {
			batch, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{d}, DigestFunction: repb.DigestFunction_SHA256, AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_ZSTD}})
			if err != nil {
				return err
			}
			if len(batch.Responses) != 1 {
				return fmt.Errorf("compressed batch count")
			}
			r := batch.Responses[0]
			data := r.Data
			if r.Compressor == repb.Compressor_ZSTD {
				data, err = harness.Decompress(data)
				if err != nil {
					return err
				}
			}
			if r.GetStatus().GetCode() != 0 || !proto.Equal(r.Digest, d) || !bytes.Equal(data, want) {
				return fmt.Errorf("compressed BatchRead %s", k)
			}
			for _, compressed := range []bool{false, true} {
				req := field(nil, 1, []byte(harness.ResourceName(instance, d, compressed)))
				if !compressed && len(want) > 1 {
					req = number(number(req, 2, 1), 3, uint64(len(want)-1))
				}
				got, err := streamBytes(ctx, c, "/google.bytestream.ByteStream/Read", req)
				if manifestOnly && !compressed && c == cs[0] {
					if status.Code(err) != codes.NotFound {
						return fmt.Errorf("known limited manifest read boundary: %v", err)
					}
					continue
				}
				if err != nil {
					return fmt.Errorf("ranged/compressed %s compressed=%t endpoint=%s: %w", k, compressed, c.Conn.Target(), err)
				}
				expected := want
				if compressed {
					got, err = harness.Decompress(got)
					if err != nil {
						return err
					}
				} else if len(want) > 1 {
					expected = want[1:]
				}
				if !bytes.Equal(got, expected) {
					return fmt.Errorf("ranged/compressed bytes %s", k)
				}
			}
			var raw []byte
			rn := number(number(field(nil, 1, dg), 4, 2), 5, 1)
			err = c.Conn.Invoke(ctx, "/cache.service.Cache/GetMetadata", field(nil, 2, rn), &raw, grpc.ForceCodec(wireCodec{}))
			if manifestOnly && c == cs[0] {
				if status.Code(err) != codes.NotFound {
					return fmt.Errorf("known app manifest cache metadata boundary: %v", err)
				}
			} else {
				if err != nil {
					return fmt.Errorf("cache service metadata: %w", err)
				}
				size, e := scalar(raw, 5)
				if e != nil || int64(size) != d.SizeBytes {
					return fmt.Errorf("cache service digest size")
				}
			}
			err = c.Conn.Invoke(ctx, "/google.bytestream.ByteStream/QueryWriteStatus", field(nil, 1, []byte(harness.UploadResourceName(instance, d, false))), &raw, grpc.ForceCodec(wireCodec{}))
			if status.Code(err) != codes.Unimplemented {
				return fmt.Errorf("QueryWriteStatus boundary: %v", err)
			}
			if _, ok := m.mappings[k]; !ok {
				_, err := c.CAS.SplitBlob(ctx, &repb.SplitBlobRequest{BlobDigest: d, DigestFunction: repb.DigestFunction_SHA256})
				if status.Code(err) != codes.NotFound {
					return fmt.Errorf("flat SplitBlob %s: %v", k, err)
				}
			}
		}
		uri := "bytestream://app:1985/blobs/" + k
		if bytes.HasPrefix(want, []byte{'P', 'K', 3, 4}) {
			if err := checkZip(ctx, f, cs[0], uri, want); err != nil {
				return err
			}
		}
		for _, method := range []string{"GetFile", "GetFileRange"} {
			if len(want) == 0 && method == "GetFileRange" {
				continue
			}
			payload := map[string]any{"uri": uri}
			if method == "GetFileRange" {
				payload["end"] = len(want)
			}
			body, _ := json.Marshal(payload)
			req, _ := http.NewRequestWithContext(ctx, "POST", f.Web.URL+"/api/v1/"+method, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			rsp, err := f.Web.Client.Do(req)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			if err != nil {
				return err
			}
			if manifestOnly && method == "GetFileRange" {
				if rsp.StatusCode != 500 || !bytes.Contains(data, []byte("NotFound")) {
					return fmt.Errorf("known REST manifest range boundary: status %d %s", rsp.StatusCode, data)
				}
				continue
			}
			if rsp.StatusCode != 200 {
				return fmt.Errorf("REST %s status %d: %s", method, rsp.StatusCode, data)
			}
			if method == "GetFileRange" {
				var decoded struct {
					Data []byte `json:"data"`
				}
				if err := json.Unmarshal(data, &decoded); err != nil {
					return err
				}
				data = decoded.Data
			}
			if !bytes.Equal(data, want) {
				return fmt.Errorf("REST %s bytes %s", method, k)
			}
		}
		got, err := streamBytes(ctx, cs[0], "/api.v1.ApiService/GetFile", field(nil, 1, []byte(uri)))
		if err != nil {
			return fmt.Errorf("GetFile: %w", err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("GetFile bytes %s", k)
		}
		if len(want) > 0 {
			var raw []byte
			err = cs[0].Conn.Invoke(ctx, "/api.v1.ApiService/GetFileRange", number(field(nil, 1, []byte(uri)), 3, uint64(len(want))), &raw, grpc.ForceCodec(wireCodec{}))
			if manifestOnly {
				if status.Code(err) != codes.NotFound {
					return fmt.Errorf("known GetFileRange boundary: %v", err)
				}
			} else {
				if err != nil {
					return fmt.Errorf("GetFileRange: %w", err)
				}
				fs, err := fields(raw, 1)
				if err != nil {
					return err
				}
				if len(fs) != 1 || !bytes.Equal(fs[0], want) {
					return fmt.Errorf("GetFileRange bytes %s", k)
				}
			}
		}
		if manifestOnly {
			rn := number(number(field(nil, 1, dg), 4, 2), 5, 1)
			var raw []byte
			err := cs[0].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(nil, 2, rn), &raw, grpc.ForceCodec(wireCodec{}))
			if status.Code(err) != codes.NotFound {
				return fmt.Errorf("known manifest metadata boundary: %v", err)
			}
			_, err = f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "cacheType": "CAS", "digestFunction": "SHA256"}})
			if err == nil || !strings.Contains(err.Error(), "NotFound") {
				return fmt.Errorf("known HTTP manifest metadata boundary: %v", err)
			}
		} else {
			rn := number(number(field(nil, 1, dg), 4, 2), 5, 1)
			var raw []byte
			err = cs[0].Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetCacheMetadata", field(nil, 2, rn), &raw, grpc.ForceCodec(wireCodec{}))
			if err != nil {
				return fmt.Errorf("metadata %s: %w", k, err)
			}
			size, err := scalar(raw, 5)
			if err != nil || int64(size) != d.SizeBytes {
				return fmt.Errorf("metadata digest size %s: %d", k, size)
			}
			res, err := f.Web.RPC("GetCacheMetadata", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "resourceName": map[string]any{"digest": map[string]any{"hash": d.Hash, "sizeBytes": strconv.FormatInt(d.SizeBytes, 10)}, "cacheType": "CAS", "digestFunction": "SHA256"}})
			if err != nil {
				return err
			}
			for fieldNum, jsonName := range map[protowire.Number]string{2: "storedSizeBytes", 4: "lastModifyUsec", 5: "digestSizeBytes"} {
				value, e := scalar(raw, fieldNum)
				if e != nil {
					return e
				}
				if jsonInteger(res[jsonName]) != strconv.FormatUint(value, 10) {
					return fmt.Errorf("HTTP/grpc metadata %s disagreement for %s", jsonName, k)
				}
			}
			if fmt.Sprint(res["digestSizeBytes"]) != strconv.FormatInt(d.SizeBytes, 10) {
				return fmt.Errorf("HTTP metadata %s: %v", k, res)
			}
		}
		for _, path := range []string{"/file/download", "/file/view"} {
			q := url.Values{"bytestream_url": {uri}, "request_context": {base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID)))}, "filename": {"structure.bin"}}
			req, err := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path+"?"+q.Encode(), nil)
			if err != nil {
				return err
			}
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			rsp, err := f.Web.Client.Do(req)
			if err != nil {
				return err
			}
			got, err := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			if err != nil {
				return err
			}
			if rsp.StatusCode != 200 || !bytes.Equal(got, want) {
				return fmt.Errorf("%s %s status %d byte mismatch", path, k, rsp.StatusCode)
			}
		}
	}
	return m.checkCLI(ctx, f)
}

func checkZip(ctx context.Context, f *harness.Fixture, c *harness.Clients, uri string, blob []byte) error {
	archive, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return err
	}
	var raw []byte
	err = c.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetZipManifest", field(nil, 2, []byte(uri)), &raw, grpc.ForceCodec(wireCodec{}))
	if err != nil {
		return fmt.Errorf("zip manifest: %w", err)
	}
	manifests, err := fields(raw, 2)
	if err != nil || len(manifests) != 1 {
		return fmt.Errorf("zip manifest cardinality")
	}
	entries, err := fields(manifests[0], 1)
	if err != nil {
		return err
	}
	if len(entries) != len(archive.File) {
		return fmt.Errorf("zip entry count")
	}
	res, err := f.Web.RPC("GetZipManifest", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "uri": uri})
	if err != nil {
		return err
	}
	manifest, ok := res["manifest"].(map[string]any)
	if !ok {
		return fmt.Errorf("HTTP zip manifest shape")
	}
	httpEntries, ok := manifest["entry"].([]any)
	if !ok || len(httpEntries) != len(entries) {
		return fmt.Errorf("HTTP zip membership")
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		names, err := fields(entry, 1)
		if err != nil || len(names) != 1 {
			return fmt.Errorf("zip name")
		}
		name := string(names[0])
		if seen[name] {
			return fmt.Errorf("duplicate zip entry")
		}
		seen[name] = true
		var file *zip.File
		for _, v := range archive.File {
			if v.Name == name {
				file = v
			}
		}
		if file == nil {
			return fmt.Errorf("unexpected zip entry %s", name)
		}
		for fieldNum, expectedValue := range map[protowire.Number]uint64{3: file.CompressedSize64, 6: 2, 7: uint64(file.CRC32)} {
			value, e := scalar(entry, fieldNum)
			if e != nil || value != expectedValue {
				return fmt.Errorf("zip model field %d for %s: %d want %d", fieldNum, name, value, expectedValue)
			}
		}
		offset, e := file.DataOffset()
		if e != nil {
			return e
		}
		value, e := scalar(entry, 5)
		if e != nil || offset <= 0 || value != 0 {
			return fmt.Errorf("known ZIP contentOffset boundary: got %d, true offset %d", value, offset)
		}
		size, err := scalar(entry, 2)
		if err != nil || size != file.UncompressedSize64 {
			return fmt.Errorf("zip size")
		}
		reader, err := file.Open()
		if err != nil {
			return err
		}
		want, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return err
		}
		found := false
		for _, e := range httpEntries {
			v, ok := e.(map[string]any)
			if !ok {
				return fmt.Errorf("HTTP zip entry shape")
			}
			if v["name"] == name {
				if found {
					return fmt.Errorf("duplicate HTTP zip name")
				}
				found = true
				for fieldNum, jsonName := range map[protowire.Number]string{2: "uncompressedSize", 3: "compressedSize", 4: "headerOffset", 5: "contentOffset", 6: "compression", 7: "crc32"} {
					if fieldNum == 6 {
						continue
					}
					value, err := scalar(entry, fieldNum)
					if err != nil {
						return err
					}
					if value != 0 && jsonInteger(v[jsonName]) != strconv.FormatUint(value, 10) {
						return fmt.Errorf("HTTP zip %s %s mismatch", name, jsonName)
					}
				}
			}
		}
		if !found {
			return fmt.Errorf("missing HTTP zip entry")
		}
		for _, path := range []string{"/file/download", "/file/view"} {
			q := url.Values{"bytestream_url": {uri}, "z": {base64.StdEncoding.EncodeToString(entry)}, "request_context": {base64.StdEncoding.EncodeToString(field(nil, 2, []byte(f.GroupID)))}}
			req, _ := http.NewRequestWithContext(ctx, "GET", f.Web.URL+path+"?"+q.Encode(), nil)
			req.Header.Set("x-buildbuddy-api-key", f.APIKey)
			rsp, err := f.Web.Client.Do(req)
			if err != nil {
				return err
			}
			got, err := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			if err != nil {
				return err
			}
			if rsp.StatusCode != 200 || !bytes.Equal(got, want) {
				return fmt.Errorf("zip extraction %s %s status %d", path, name, rsp.StatusCode)
			}
		}
	}
	return nil
}

func jsonInteger(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return fmt.Sprint(v)
}

func (m *model) rawCLI(ctx context.Context, f *harness.Fixture, bb, k string, want []byte, component harness.Component) error {
	out, err := os.MkdirTemp("", "structure-raw-")
	if err != nil {
		return err
	}
	dest := filepath.Join(out, "blob")
	cmd := exec.CommandContext(ctx, bb, "download", k, "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+dest)
	cmd.Env = []string{"HOME=" + out, "PATH=/usr/local/bin:/usr/bin:/bin"}
	log, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(out)
		return fmt.Errorf("CLI raw: %w %s", err, log)
	}
	got, err := os.ReadFile(dest)
	os.RemoveAll(out)
	if err != nil || !bytes.Equal(got, want) {
		return fmt.Errorf("CLI raw bytes %s", k)
	}
	if dir, ok := m.dirs[k]; ok {
		out, err := os.MkdirTemp("", "structure-json-")
		if err != nil {
			return err
		}
		dest := filepath.Join(out, "directory.json")
		cmd := exec.CommandContext(ctx, bb, "download", k, "--type=Directory", "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_file="+dest)
		cmd.Env = []string{"HOME=" + out, "PATH=/usr/local/bin:/usr/bin:/bin"}
		log, err := cmd.CombinedOutput()
		if err != nil {
			os.RemoveAll(out)
			return fmt.Errorf("CLI Directory JSON: %w %s", err, log)
		}
		data, err := os.ReadFile(dest)
		os.RemoveAll(out)
		if err != nil {
			return err
		}
		decoded := &repb.Directory{}
		if err := protojson.Unmarshal(data, decoded); err != nil {
			return err
		}
		if !proto.Equal(dir, decoded) {
			return fmt.Errorf("CLI Directory JSON model mismatch %s", k)
		}
	}

	return nil
}
func (m *model) directoryCLI(ctx context.Context, f *harness.Fixture, bb string, root *repb.Digest, component harness.Component) error {
	parent, err := os.MkdirTemp("", "structures-cli-")
	out := filepath.Join(parent, "payload")
	if err == nil {
		err = os.Mkdir(out, 0755)
	}
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bb, "download", key(root), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey, "--output_directory="+out)
	cmd.Env = []string{"HOME=" + parent, "PATH=/usr/local/bin:/usr/bin:/bin"}
	log, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(out)
		return fmt.Errorf("CLI: %w %s", err, log)
	}
	expected := map[string]*repb.FileNode{}
	expectedDirs := map[string]bool{".": true}
	var walk func(*repb.Digest, string)
	walk = func(d *repb.Digest, p string) {
		dir := m.dirs[key(d)]
		if p != "" {
			expectedDirs[p] = true
		}
		for _, file := range dir.Files {
			expected[filepath.Join(p, file.Name)] = file
		}
		for _, sub := range dir.Directories {
			walk(sub.Digest, filepath.Join(p, sub.Name))
		}
	}
	walk(root, "")
	for p, file := range expected {
		got, err := os.ReadFile(filepath.Join(out, p))
		if err != nil || !bytes.Equal(got, m.blobs[key(file.Digest)]) {
			os.RemoveAll(out)
			return fmt.Errorf("CLI file %s: %v", p, err)
		}
		info, err := os.Stat(filepath.Join(out, p))
		if err != nil {
			return err
		}
		if (info.Mode()&0111 != 0) != file.IsExecutable {
			return fmt.Errorf("CLI executable %s", p)
		}
	}
	count := 0
	err = filepath.Walk(out, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(out, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if !expectedDirs[rel] {
				return fmt.Errorf("unexpected CLI directory %s", rel)
			}
			return nil
		}
		if _, ok := expected[rel]; !ok {
			return fmt.Errorf("unexpected CLI path %s", rel)
		}
		count++
		return nil
	})
	os.RemoveAll(parent)
	if err != nil {
		return err
	}
	if count != len(expected) {
		return fmt.Errorf("CLI exact file membership")
	}
	return nil
}
func (m *model) checkCLI(ctx context.Context, f *harness.Fixture) error {
	bb, err := exec.LookPath("bb")
	if err != nil {
		return fmt.Errorf("required CLI: %w", err)
	}
	var tasks []func() error
	keys := make([]string, 0, len(m.blobs))
	for k := range m.blobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			tasks = append(tasks, func() error { return m.rawCLI(ctx, f, bb, k, m.blobs[k], component) })
		}
	}
	for _, root := range m.roots {
		for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
			tasks = append(tasks, func() error { return m.directoryCLI(ctx, f, bb, root, component) })
		}
	}
	// Bound read-only subprocess concurrency; each owns a disposable HOME/output.
	jobs := make(chan int, len(tasks))
	results := make([]error, len(tasks))
	var workers sync.WaitGroup
	for n := 0; n < 4; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				results[i] = tasks[i]()
			}
		}()
	}
	for i := range tasks {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	for _, err := range results {
		if err != nil {
			return err
		}
	}
	return nil
}
