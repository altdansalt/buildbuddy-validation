package convergence_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
	"strings"
	"testing"
	"time"
)

func TestZipManifestContentOffset(t *testing.T) {
	for attempt := 0; attempt < 2; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var buffer bytes.Buffer
			writer := zip.NewWriter(&buffer)
			entry, err := writer.CreateHeader(&zip.FileHeader{Name: "file.txt", Method: zip.Store})
			require.NoError(t, err)
			_, err = entry.Write([]byte("valid zip payload"))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			reader, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
			require.NoError(t, err)
			require.Len(t, reader.File, 1)
			offset, err := reader.File[0].DataOffset()
			require.NoError(t, err)
			require.Positive(t, offset)
			app := f.Clients(harness.App)
			d, err := app.Upload(ctx, "zip-offset", buffer.Bytes())
			require.NoError(t, err)
			uri := "bytestream://" + f.APIKey + "@" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + harness.ResourceName("zip-offset", d, false)
			out := &emptypb.Empty{}
			require.NoError(t, app.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/GetZipManifest", wire(field(nil, 2, []byte(uri))), out))
			manifests := fields(t, out.ProtoReflect().GetUnknown(), 2)
			require.Len(t, manifests, 1)
			entries := fields(t, manifests[0], 1)
			require.Len(t, entries, 1)
			require.Equal(t, "file.txt", string(fields(t, entries[0], 1)[0]))
			require.Equal(t, uint64(reader.File[0].UncompressedSize64), integer(t, entries[0], 2))
			httpMessages, code := frontendHTTP(t, ctx, f, "GetZipManifest", field(nil, 2, []byte(uri)))
			require.Equal(t, "0", code)
			require.Len(t, httpMessages, 1)
			httpEntries := fields(t, fields(t, httpMessages[0], 2)[0], 1)
			require.Len(t, httpEntries, 1)
			t.Logf("ZIP manifest content_offset gRPC=%d HTTP=%d want=%d; persisted ZIP validated by archive/zip", integer(t, entries[0], 5), integer(t, httpEntries[0], 5), offset)
			require.Equal(t, uint64(offset), integer(t, entries[0], 5), "documented absolute content offset was omitted")
			require.Equal(t, uint64(offset), integer(t, httpEntries[0], 5))
		})
	}
}
