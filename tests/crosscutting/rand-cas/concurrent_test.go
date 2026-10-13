package randcas_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Prepopulating the immutable model makes every concurrent read and presence
// check deterministic, including while another client rewrites the same digest.
func TestConcurrentOverlappingCAS(t *testing.T) {
	t.Logf("seed=%d", *seed)
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	rng := rand.New(rand.NewSource(*seed))
	data := make([][]byte, 4)
	for i := range data {
		data[i] = make([]byte, 1+rng.Intn(128<<10))
		rng.Read(data[i])
		_, err := clients[i%2].Upload(ctx, "concurrent", data[i])
		check(t, err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			// Fatal assertions are legal in this worker: Goexit still runs wg.Done.
			r := rand.New(rand.NewSource(*seed + int64(worker+1)*100003))
			<-start
			for step := 0; step < 40; step++ {
				i := r.Intn(len(data))
				c := clients[r.Intn(2)]
				d := harness.Digest(data[i])
				t.Logf("seed=%d worker=%d step=%d blob=%d", *seed, worker, step, i)
				switch r.Intn(4) {
				case 0:
					write(t, ctx, c, fmt.Sprintf("concurrent/uploads/%08x-0000-4000-8000-%012x/blobs/%s/%d", worker, step, d.Hash, d.SizeBytes), data[i], false, r)
				case 1:
					off := r.Intn(len(data[i]))
					limit := 1 + r.Intn(len(data[i])-off)
					got, err := read(ctx, c, harness.ResourceName("concurrent", d, false), int64(off), int64(limit))
					check(t, err)
					equal(t, data[i][off:off+limit], got, "concurrent range")
				case 2:
					res, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: "concurrent", BlobDigests: []*repb.Digest{d, d}})
					check(t, err)
					if len(res.MissingBlobDigests) != 0 {
						t.Errorf("seed=%d worker=%d step=%d stored digest missing: %v", *seed, worker, step, res)
					}
				case 3:
					res, err := c.CAS.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: "concurrent", Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: data[i]}}})
					check(t, err)
					if len(res.Responses) != 1 || res.Responses[0].GetStatus().GetCode() != 0 || res.Responses[0].GetDigest().GetHash() != d.Hash {
						t.Errorf("seed=%d update=%v", *seed, res)
					}
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	for _, c := range clients {
		for _, b := range data {
			got, err := c.Read(ctx, "concurrent", harness.Digest(b))
			check(t, err)
			equal(t, b, got, "final concurrent read")
		}
	}
}

func TestInterruptedUploads(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
	for endpoint, c := range clients {
		t.Run([]string{"app", "cacheproxy"}[endpoint], func(t *testing.T) {
			r := rand.New(rand.NewSource(*seed + int64(endpoint)*100003))
			for trial := 0; trial < 8; trial++ {
				data := make([]byte, 65536+r.Intn(256<<10))
				r.Read(data)
				d := harness.Digest(data)
				n := fmt.Sprintf("interrupted/uploads/%08x-0000-4000-8000-%012x/blobs/%s/%d", endpoint, trial, d.Hash, d.SizeBytes)
				for interruption := 0; interruption < 3; interruption++ {
					cut := 1 + r.Intn(len(data)-1)
					s, err := c.ByteStream.Write(ctx)
					check(t, err)
					check(t, s.Send(&bspb.WriteRequest{ResourceName: n, Data: data[:cut]}))
					// Half-close without finish_write; drain the RPC to ensure teardown,
					// rather than guessing whether the server received a cancelled Send.
					_, err = s.CloseAndRecv()
					if err != nil && err != io.EOF {
						check(t, err)
					}
					q, err := c.ByteStream.QueryWriteStatus(ctx, &bspb.QueryWriteStatusRequest{ResourceName: n})
					off := int64(0)
					switch status.Code(err) {
					case codes.Unimplemented, codes.NotFound:
						t.Logf("seed=%d trial=%d interruption=%d cut=%d query=%s; restart at zero", *seed, trial, interruption, cut, status.Code(err))
					case codes.OK:
						if q.Complete || q.CommittedSize < 0 || q.CommittedSize > int64(cut) {
							t.Fatalf("invalid partial status: %v", q)
						}
						off = q.CommittedSize
						t.Logf("seed=%d trial=%d interruption=%d cut=%d resume=%d", *seed, trial, interruption, cut, off)
					default:
						check(t, err)
					}
					// Complete after the final interruption, using the acknowledged offset.
					if interruption == 2 {
						s, err = c.ByteStream.Write(ctx)
						check(t, err)
						for off < int64(len(data)) {
							end := off + int64(1+r.Intn(32<<10))
							if end > int64(len(data)) {
								end = int64(len(data))
							}
							err = s.Send(&bspb.WriteRequest{ResourceName: n, WriteOffset: off, Data: data[off:end], FinishWrite: end == int64(len(data))})
							if err == io.EOF {
								break
							}
							check(t, err)
							off = end
						}
						res, err := s.CloseAndRecv()
						check(t, err)
						if res.CommittedSize != int64(len(data)) {
							t.Fatalf("committed=%d want=%d", res.CommittedSize, len(data))
						}
					}
				}
				for _, peer := range clients {
					got, err := peer.Read(ctx, "interrupted", d)
					check(t, err)
					equal(t, data, got, "interrupted upload")
				}
			}
		})
	}
}
