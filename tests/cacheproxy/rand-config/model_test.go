package randconfig_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var seed = flag.Int64("randconfig.seed", 314, "First reproducible random seed")
var iterations = flag.Int("randconfig.iterations", 180, "Operations per configuration (90-second RPC budget)")

type key struct {
	instance string
	slot     int
}
type result struct {
	exit           int32
	stdout, stderr []byte
}

// Configuration choices and workload choices use independent seeded RNGs.
var configurations = flag.Int("randconfig.configurations", 6, "Number of fresh configurations (maximum 20)")

func TestRandomConfigurations(t *testing.T) {
	if *iterations < 1 || *configurations < 1 || *configurations > 20 {
		t.Fatal("iterations must be positive; configurations must be between 1 and 20")
	}
	t.Logf("randconfig.seed=%d iterations=%d configurations=%d", *seed, *iterations, *configurations)
	for i := 0; i < *configurations; i++ {
		s := *seed + int64(i)
		t.Run(fmt.Sprintf("seed_%d", s), func(t *testing.T) {
			rng := rand.New(rand.NewSource(s))
			// Alternate tiny and roomy caches so every default run covers both.
			sizes := []int64{8 << 10, 32 << 10, 128 << 10}
			size := sizes[rng.Intn(len(sizes))]
			if i%2 == 1 {
				size = 64 << 20
			}
			compression := []int64{0, 1024, 4096, 1 << 30}[rng.Intn(4)]
			inline := []int64{0, 128, 4096}[rng.Intn(3)]
			ttl := []string{"0s", "250ms", "5s"}[rng.Intn(3)]
			args := []string{
				fmt.Sprintf("--cache_proxy.cache_action_results=%t", rng.Intn(2) == 0),
				fmt.Sprintf("--cache_proxy.skip_write_validation=%t", rng.Intn(2) == 0),
				fmt.Sprintf("--cache_proxy.disable_cdc=%t", rng.Intn(2) == 0),
				"--cache_proxy.find_missing_blobs_cache_ttl=" + ttl,
				fmt.Sprintf("--cache_proxy.find_missing_blobs_cache_size_entries=%d", 1+rng.Intn(16)),
			}
			t.Logf("seed=%d max_size_bytes=%d compression_threshold=%d inline_threshold=%d args=%v", s, size, compression, inline, args)
			f := harness.Start(t, harness.Options{
				Components: []harness.Component{harness.App, harness.CacheProxy},
				Args:       map[harness.Component][]string{harness.CacheProxy: args},
				Config: map[harness.Component]harness.Config{harness.CacheProxy: {
					"cache": harness.Config{"max_size_bytes": size, "pebble": harness.Config{
						"min_bytes_auto_zstd_compression": compression,
						"max_inline_file_size_bytes":      inline,
						"min_eviction_age":                "0s",
						"include_metadata_size":           true,
					}},
				}},
			})
			clients := []*harness.Clients{f.Clients(harness.App), f.Clients(harness.CacheProxy)}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			// Oversubscribe even the largest tiny configuration with unique,
			// incompressible blobs, then keep exercising reads during eviction.
			pressure := make([][]byte, 24)
			for j := range pressure {
				pressure[j] = make([]byte, 32<<10)
				rng.Read(pressure[j])
				if _, err := clients[1].Write(ctx, "pressure", pressure[j], j%2 == 0); err != nil {
					t.Fatalf("seed=%d pressure step=%d: %v", s, j, err)
				}
			}
			runModel(t, ctx, clients, s, make(map[string]bool))
			// The authoritative app remains roomy: every pressure blob must still
			// round-trip through the proxy after local eviction and model traffic.
			for j, data := range pressure {
				got, err := clients[1].Read(ctx, "pressure", harness.Digest(data))
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("seed=%d pressure read=%d digest=%v err=%v", s, j, harness.Digest(got), err)
				}
			}
		})
	}
}

func runModel(t *testing.T, ctx context.Context, clients []*harness.Clients, s int64, cas map[string]bool) {
	t.Helper()
	rng := rand.New(rand.NewSource(s))
	instances := []string{"", "model-a", "model/nested-b"}
	// Shared payload pool deliberately repeats digests across instances; the
	// empty blob is universally available without an upload under REAPI.
	blobs := make([][]byte, 12)
	sizes := []int{0, 1, 2, 31, 4095, 4096, 4097, 65536}
	seen := map[string]bool{harness.Digest(nil).Hash: true}
	for i := 1; i < len(blobs); i++ {
		blobs[i] = make([]byte, sizes[1+rng.Intn(len(sizes)-1)])
		for {
			rng.Read(blobs[i])
			hash := harness.Digest(blobs[i]).Hash
			if !seen[hash] {
				seen[hash] = true
				break
			}
		}
	}
	ac := make(map[key]result)
	actions := make(map[key]*repb.Digest)
	step := -1
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed=%d step=%d: %s", s, step, fmt.Sprintf(format, args...))
	}
	checkErr := func(err error) {
		if err != nil {
			fail("unexpected RPC error: %v", err)
		}
	}
	checkRead := func(c *harness.Clients, k key, required bool) {
		got, err := c.Read(ctx, k.instance, harness.Digest(blobs[k.slot]))
		if status.Code(err) == codes.NotFound && !required {
			return
		}
		checkErr(err)
		if !bytes.Equal(got, blobs[k.slot]) {
			fail("CAS instance=%q slot=%d got %v want %v", k.instance, k.slot, harness.Digest(got), harness.Digest(blobs[k.slot]))
		}
		if !cas[harness.Digest(blobs[k.slot]).Hash] && k.slot != 0 {
			fail("CAS returned unwritten instance=%q slot=%d", k.instance, k.slot)
		}
	}
	checkAC := func(c *harness.Clients, k key, required bool) {
		d := actions[k]
		if d == nil {
			d = harness.Digest([]byte(fmt.Sprintf("missing-action-%d-%s-%d", s, k.instance, k.slot)))
		}
		got, err := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: k.instance, ActionDigest: d, DigestFunction: repb.DigestFunction_SHA256})
		if status.Code(err) == codes.NotFound && !required {
			return
		}
		checkErr(err)
		want, ok := ac[k]
		if !ok || got.ExitCode != want.exit || !bytes.Equal(got.StdoutRaw, want.stdout) || !bytes.Equal(got.StderrRaw, want.stderr) {
			fail("AC instance=%q slot=%d got=%v want=%+v exists=%t", k.instance, k.slot, got, want, ok)
		}
	}
	counts := make([]int, 7)
	for step = 0; step < *iterations; step++ {
		if ctx.Err() != nil {
			fail("operation budget exhausted: %v", ctx.Err())
		}
		k := key{instances[rng.Intn(len(instances))], rng.Intn(len(blobs))}
		c := clients[rng.Intn(len(clients))]
		op := rng.Intn(len(counts))
		counts[op]++
		t.Logf("seed=%d step=%d op=%d endpoint=%t instance=%q slot=%d", s, step, op, c == clients[1], k.instance, k.slot)
		switch op {
		case 0, 1:
			var err error
			if op == 0 {
				_, err = c.Upload(ctx, k.instance, blobs[k.slot])
			} else {
				_, err = c.Write(ctx, k.instance, blobs[k.slot], rng.Intn(2) == 0)
			}
			checkErr(err)
			cas[harness.Digest(blobs[k.slot]).Hash] = true
			for _, peer := range clients {
				checkRead(peer, k, true)
			}
		case 2:
			checkRead(c, k, false)
		case 3:
			// Distinct requested digests, checked as a set (response order is unspecified).
			requested := make(map[string]key)
			ds := []*repb.Digest{}
			for j := 0; j < 4; j++ {
				q := key{k.instance, (k.slot + j) % len(blobs)}
				d := harness.Digest(blobs[q.slot])
				requested[d.Hash] = q
				ds = append(ds, d)
			}
			resp, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: k.instance, BlobDigests: ds, DigestFunction: repb.DigestFunction_SHA256})
			checkErr(err)
			missing := make(map[string]bool)
			for _, d := range resp.MissingBlobDigests {
				q, ok := requested[d.Hash]
				if !ok || missing[d.Hash] || d.SizeBytes != int64(len(blobs[q.slot])) {
					fail("invalid FindMissing response %v", resp)
				}
				missing[d.Hash] = true
			}
			for hash, q := range requested {
				if !missing[hash] && !cas[hash] && q.slot != 0 {
					fail("FindMissing reported unwritten blob present: %+v", q)
				}
			}
		case 4:
			resp, err := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: k.instance, Digests: []*repb.Digest{harness.Digest(blobs[k.slot])}, DigestFunction: repb.DigestFunction_SHA256})
			checkErr(err)
			if len(resp.Responses) != 1 {
				fail("BatchRead response count: %v", resp)
			}
			r := resp.Responses[0]
			if r.Digest.GetHash() != harness.Digest(blobs[k.slot]).Hash || r.Digest.GetSizeBytes() != int64(len(blobs[k.slot])) {
				fail("BatchRead wrong digest: %v", r)
			}
			if codes.Code(r.GetStatus().GetCode()) == codes.NotFound {
				continue
			}
			if r.GetStatus().GetCode() != 0 || r.Compressor != repb.Compressor_IDENTITY || !bytes.Equal(r.Data, blobs[k.slot]) || (!cas[harness.Digest(blobs[k.slot]).Hash] && k.slot != 0) {
				fail("BatchRead disagrees with model: %v", r)
			}
		case 5:
			if actions[k] == nil {
				cd, err := c.UploadProto(ctx, k.instance, &repb.Command{Arguments: []string{"echo", fmt.Sprintf("seed-%d-slot-%d", s, k.slot)}})
				checkErr(err)
				cas[cd.Hash] = true
				root, err := c.UploadProto(ctx, k.instance, &repb.Directory{})
				checkErr(err)
				cas[root.Hash] = true
				ad, err := c.UploadProto(ctx, k.instance, &repb.Action{CommandDigest: cd, InputRootDigest: root})
				checkErr(err)
				actions[k] = ad
				cas[ad.Hash] = true
			}
			want := result{int32(rng.Intn(4)), []byte(fmt.Sprintf("stdout seed=%d revision=%d", s, step)), []byte(fmt.Sprintf("stderr %d", rng.Int63()))}
			got, err := c.AC.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: k.instance, ActionDigest: actions[k], DigestFunction: repb.DigestFunction_SHA256, ActionResult: &repb.ActionResult{ExitCode: want.exit, StdoutRaw: want.stdout, StderrRaw: want.stderr}})
			checkErr(err)
			if got.ExitCode != want.exit || !bytes.Equal(got.StdoutRaw, want.stdout) || !bytes.Equal(got.StderrRaw, want.stderr) {
				fail("UpdateActionResult changed result: %v", got)
			}
			ac[k] = want
			for _, peer := range clients {
				checkAC(peer, k, true)
			}
		case 6:
			checkAC(c, k, false)
		}
	}
	t.Logf("seed=%d operation counts=%v", s, counts)
	// A final sweep detects delayed corruption and stale proxy entries, including
	// entries last updated through the other endpoint.
	for _, instance := range instances {
		for slot := range blobs {
			k := key{instance, slot}
			for _, c := range clients {
				checkRead(c, k, false)
				checkAC(c, k, false)
			}
		}
	}
}
