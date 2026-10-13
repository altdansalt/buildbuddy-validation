package orgkeys

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/protobuf/encoding/protowire"
	"testing"
	"time"
)

// Modern visibility is a valid, documented replacement for the deprecated bool.
// This isolated contract test must fail until the released handler honors it.
func TestModernVisibility(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "visibility-admin", []string{"ORG_ADMIN"}))
	rc := msg(1, str(2, f.GroupID))
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			req := append(append([]byte(nil), rc...), str(3, "modern-visibility")...)
			req = append(req, num(4, 1)...)
			if operation == "create" {
				req = append(req, num(8, 1)...)
				req = append(req, num(8, 2)...)
			}
			out, err := rpc(ctx, admin.Conn, "CreateApiKey", req)
			require(t, err)
			id := decode(first(fields(out), 2)).id
			if operation == "update" {
				req = append(append([]byte(nil), rc...), str(2, id)...)
				req = append(req, str(3, "modern-visibility")...)
				req = append(req, num(4, 1)...)
				req = append(req, num(6, 1)...)
				req = append(req, num(6, 2)...)
				_, err = rpc(ctx, admin.Conn, "UpdateApiKey", req)
				require(t, err)
			}
			for _, method := range []string{"GetApiKey", "GetApiKeys"} {
				req = append([]byte(nil), rc...)
				if method == "GetApiKey" {
					req = append(req, str(2, id)...)
				}
				out, err = rpc(ctx, admin.Conn, method, req)
				require(t, err)
				for _, b := range fields(out)[2] {
					if decode(b).id != id {
						continue
					}
					// Repeated enum values may be packed or unpacked.
					var mask uint64
					for _, b := range fields(b)[10] {
						for len(b) > 0 {
							v, n := protowire.ConsumeVarint(b)
							mask |= v
							b = b[n:]
						}
					}
					if mask != 3 {
						t.Errorf("%s %s visibility mask=%d want 3 (developers + admins)", operation, method, mask)
					}
				}
			}
		})
	}
}

// A future expiry is valid input; metadata readers must expose the stored expiry.
func TestExpiryProjection(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "expiry-admin", []string{"ORG_ADMIN"}))
	rc := msg(1, str(2, f.GroupID))
	req := append(append([]byte(nil), rc...), str(3, "expiring")...)
	req = append(req, num(4, 1)...)
	req = append(req, msg(7, num(1, 3600))...)
	earliest := time.Now().Add(time.Hour).UnixMicro()
	out, err := rpc(ctx, admin.Conn, "CreateApiKey", req)
	require(t, err)
	latest := time.Now().Add(time.Hour).UnixMicro()
	id := decode(first(fields(out), 2)).id
	assert := func(surface string, b []byte) {
		v, _ := protowire.ConsumeVarint(first(fields(b), 7))
		if int64(v) < earliest || int64(v) > latest {
			t.Errorf("%s expiry_usec=%d want within [%d,%d]", surface, v, earliest, latest)
		}
	}
	assert("CreateApiKey", first(fields(out), 2))
	for _, method := range []string{"GetApiKey", "GetApiKeys", "GetBazelConfig"} {
		req = append([]byte(nil), rc...)
		if method == "GetApiKey" {
			req = append(req, str(2, id)...)
		}
		out, err = rpc(ctx, admin.Conn, method, req)
		require(t, err)
		n := protowire.Number(2)
		if method == "GetBazelConfig" {
			n = 4
		}
		for _, b := range fields(out)[n] {
			if n == 4 {
				b = first(fields(b), 1)
			}
			if decode(b).id == id {
				assert(method, b)
			}
		}
	}
}
