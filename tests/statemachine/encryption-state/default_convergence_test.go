package encryption_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/protobuf/proto"
)

// Keep production authentication and encryption-key TTLs. Prime both keys and
// both endpoints before enabling, then require new writes to use encryption.
func TestDefaultCacheConvergence(t *testing.T) {
	s := startSuite(t, false)
	s.config(false, 20*time.Second)
	s.write(100, false, false)
	rpc(t, s.ctx, s.clients[0], service+"SetEncryptionConfig", join(num(2, 1), msg(3, msg(1, msg(1, []byte("customer"))))))
	start := time.Now()
	sequence := 0
	s.converge("default auth caches encryption visibility", 6*time.Minute, func() bool {
		sequence++
		data := []byte(fmt.Sprintf("default-encrypted-%d", sequence))
		d := harness.Digest(data)
		dw, _ := proto.Marshal(d)
		req := msg(2, join(msg(1, dw), num(4, 2)))
		for _, c := range s.clients {
			_, err := c.Upload(s.ctx, "", data)
			if err != nil {
				return false
			}
			out, err := s.invoke(c, "/cache.service.Cache/GetMetadata", req)
			_, stored := field(out, 2)
			if err != nil || stored <= uint64(len(data)) {
				return false
			}
		}
		return true
	})
	t.Logf("production TTL transition observed after %s", time.Since(start))
	s.config(true, 20*time.Second)
	s.write(101, true, false)
}
