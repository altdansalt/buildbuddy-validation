package bytestream_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/altdansalt/buildbuddy-validation/harness"
	bspb "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Plans are built serially: goroutine scheduling never changes the replay data.
func chunkPlan(rng *rand.Rand, name string, wire []byte) []*bspb.WriteRequest {
	reqs := []*bspb.WriteRequest{{ResourceName: name}}
	for off := 0; off < len(wire); {
		end := off + 1 + rng.Intn(64<<10)
		if end > len(wire) {
			end = len(wire)
		}
		reqs = append(reqs, &bspb.WriteRequest{WriteOffset: int64(off), Data: wire[off:end]})
		off = end
	}
	return append(reqs, &bspb.WriteRequest{WriteOffset: int64(len(wire)), FinishWrite: true})
}

func TestRandomizedConcurrentReadersWriters(t *testing.T) {
	t.Logf("replay with -bytestream.seed=%d", *propertySeed)
	rng := rand.New(rand.NewSource(*propertySeed))
	ctx, c := setup(t)
	for round := 0; round < 8; round++ {
		data := make([]byte, 65537+rng.Intn(1<<20))
		_, _ = rng.Read(data)
		d := harness.Digest(data)
		instance := fmt.Sprintf("concurrent-property-%d-%d", *propertySeed, round)
		// Alternate absent and already published digests; both use the same digest
		// for every reader and writer within the round.
		published := round%2 == 1
		if published {
			if _, err := c.Write(ctx, instance, data, false); err != nil {
				t.Fatal(err)
			}
		}
		plans := make([][]*bspb.WriteRequest, 4)
		for i := range plans {
			compressed := i%2 == 1
			wire := data
			if compressed {
				var err error
				wire, err = harness.Compress(data)
				if err != nil {
					t.Fatal(err)
				}
			}
			plans[i] = chunkPlan(rng, harness.UploadResourceName(instance, d, compressed), wire)
		}
		type readPlan struct {
			compressed bool
			off, limit int64
		}
		readers := make([][]readPlan, 4)
		for i := range readers {
			for j := 0; j < 12; j++ {
				p := readPlan{compressed: j%2 == 1, off: int64(rng.Intn(len(data) + 1))}
				if !p.compressed {
					p.limit = int64(rng.Intn(len(data) + 1))
				}
				readers[i] = append(readers[i], p)
			}
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i, plan := range plans {
			wg.Add(1)
			go func(i int, plan []*bspb.WriteRequest) {
				defer wg.Done()
				<-start
				response, err := write(ctx, c, plan...)
				if err == nil && response.CommittedSize != int64(sumData(plan)) && !(i%2 == 1 && response.CommittedSize == -1) {
					err = fmt.Errorf("unexpected committed size %d", response.CommittedSize)
				}
				if err != nil {
					errs <- fmt.Errorf("round=%d writer=%d: %w", round, i, err)
				}
			}(i, plan)
		}
		for i, plan := range readers {
			wg.Add(1)
			go func(i int, plan []readPlan) {
				defer wg.Done()
				<-start
				seen := published
				for j, p := range plan {
					got, err := read(ctx, c, &bspb.ReadRequest{ResourceName: harness.ResourceName(instance, d, p.compressed), ReadOffset: p.off, ReadLimit: p.limit})
					if status.Code(err) == codes.NotFound && !seen && len(got) == 0 {
						continue
					}
					if err != nil {
						errs <- fmt.Errorf("round=%d reader=%d read=%d: %w", round, i, j, err)
						return
					}
					seen = true
					if p.compressed {
						got, err = harness.Decompress(got)
					}
					end := int64(len(data))
					if p.limit > 0 && p.limit < end-p.off {
						end = p.off + p.limit
					}
					if err != nil || !bytes.Equal(got, data[p.off:end]) {
						errs <- fmt.Errorf("round=%d reader=%d read=%d off=%d limit=%d compressed=%t: corrupt read (%d bytes), %v", round, i, j, p.off, p.limit, p.compressed, len(got), err)
						return
					}
				}
			}(i, plan)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		got, err := c.Read(ctx, instance, d)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("round=%d final read: bytes=%d err=%v", round, len(got), err)
		}
	}
}

func sumData(reqs []*bspb.WriteRequest) int {
	n := 0
	for _, r := range reqs {
		n += len(r.Data)
	}
	return n
}

func TestRandomizedInterruptedRestart(t *testing.T) {
	t.Logf("replay with -bytestream.seed=%d", *propertySeed)
	rng := rand.New(rand.NewSource(*propertySeed))
	ctx, c := setup(t)
	for round := 0; round < 16; round++ {
		data := make([]byte, 1+rng.Intn(1<<20))
		_, _ = rng.Read(data)
		compressed := round%2 == 1
		wire := data
		if compressed {
			var err error
			wire, err = harness.Compress(data)
			if err != nil {
				t.Fatal(err)
			}
		}
		d := harness.Digest(data)
		instance := fmt.Sprintf("restart-property-%d-%d", *propertySeed, round)
		name := harness.UploadResourceName(instance, d, compressed)
		for attempt := 0; attempt < 3; attempt++ {
			cut := rng.Intn(len(wire) + 1) // Includes all data sent without FinishWrite.
			if attempt == 0 && round%4 == 0 {
				cut = 0
			}
			if attempt == 1 && round%4 == 0 {
				cut = len(wire)
			}
			t.Logf("round=%d compressed=%t attempt=%d interrupted wire offset=%d/%d", round, compressed, attempt, cut, len(wire))
			partial, cancel := context.WithCancel(ctx)
			stream, err := c.ByteStream.Write(partial)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			plan := chunkPlan(rng, name, wire[:cut])
			for _, req := range plan[:len(plan)-1] {
				if err = stream.Send(req); err != nil {
					cancel()
					t.Fatal(err)
				}
			}
			if attempt%2 == 0 {
				cancel()
			}
			// Half-close or cancel without FinishWrite. The response status is covered
			// by the existing unfinished-response bug test; only publication matters here.
			_, _ = stream.CloseAndRecv()
			cancel()
			got, err := c.Read(ctx, instance, d)
			if status.Code(err) != codes.NotFound || len(got) != 0 {
				t.Fatalf("round=%d attempt=%d unfinished upload visible: bytes=%d err=%v", round, attempt, len(got), err)
			}
		}
		response, err := write(ctx, c, chunkPlan(rng, name, wire)...)
		if err != nil {
			t.Fatalf("round=%d restart from zero: %v", round, err)
		}
		if response.CommittedSize != int64(len(wire)) && !(compressed && response.CommittedSize == -1) {
			t.Fatalf("round=%d committed=%d want=%d", round, response.CommittedSize, len(wire))
		}
		got, err := c.Read(ctx, instance, d)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("round=%d restarted readback: bytes=%d err=%v", round, len(got), err)
		}
	}
}
