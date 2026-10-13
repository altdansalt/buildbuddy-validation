package membership

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Role capability masks from server/util/role/role.go (user-key bits only).
var roleMask = map[int]int{1: 4, 2: 1 | 4 | 8, 4: 1 | 4, 8: 0}

// storedAfterRoleChange mirrors userdb.updateUserRole: the stored key mask only
// ever shrinks; a later role upgrade does not restore dropped capabilities.
func storedAfterRoleChange(stored, newRole int) int {
	return (stored | ((stored & 1) << 2)) & roleMask[newRole]
}

// effectiveCaps mirrors the read-time membership mask in authdb: CACHE_WRITE
// implies CAS_WRITE, the role mask applies, and CACHE_WRITE displaces CAS_WRITE.
func effectiveCaps(stored, role int) int {
	if stored&1 != 0 {
		stored |= 4
	}
	stored &= roleMask[role]
	if stored&1 != 0 {
		stored ^= 4
	}
	return stored
}

func observeTargetKey(conn *grpc.ClientConn, admin, member *identity, gid string, m model) error {
	h, he := admin.web.RPC("GetUserApiKeys", map[string]any{"requestContext": admin.web.RequestContext(gid), "userId": member.uid})
	g, ge := rpc(conn, admin.manageKey, "GetUserApiKeys", join(bytesField(1, requestContext(admin.uid, gid)), text(3, member.uid)))
	if m.status != 1 {
		if he == nil || !strings.Contains(he.Error(), "PermissionDenied") || status.Code(ge) != codes.PermissionDenied {
			return fmt.Errorf("nonmember key listing HTTP=%v gRPC=%v", he, ge)
		}
	} else {
		if he != nil || ge != nil {
			return fmt.Errorf("member key listing HTTP=%v gRPC=%v", he, ge)
		}
		if len(array(h["apiKey"])) != 1 || len(g[2]) != 1 {
			return fmt.Errorf("target key count HTTP=%v gRPC=%v", h, g)
		}
		hk := object(array(h["apiKey"])[0])
		k, err := decode(first(g, 2))
		if err != nil {
			return err
		}
		if hk["id"] != member.targetID || string(first(k, 1)) != member.targetID {
			return fmt.Errorf("target key identity mismatch")
		}
		// A key requests CACHE_WRITE and CAS_WRITE; membership masks that set.
		eff := effectiveCaps(member.storedCaps, m.role)
		want := map[string]bool{}
		wantWire := map[byte]bool{}
		if eff&4 != 0 {
			want["CAS_WRITE"] = true
			wantWire[4] = true
		}
		if eff&1 != 0 {
			want["CACHE_WRITE"] = true
			wantWire[1] = true
		}
		got := map[string]bool{}
		for _, v := range array(hk["capability"]) {
			got[fmt.Sprint(v)] = true
		}
		gotWire := map[byte]bool{}
		for _, b := range k[4] {
			for _, v := range b {
				gotWire[v] = true
			}
		}
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotWire, wantWire) {
			return fmt.Errorf("key capability mask HTTP=%v gRPC=%v want %v", got, gotWire, want)
		}
		h, he = member.web.RPC("GetUserApiKey", map[string]any{"requestContext": member.web.RequestContext(gid), "apiKeyId": member.targetID})
		g, ge = rpc(conn, admin.manageKey, "GetUserApiKey", join(bytesField(1, requestContext(admin.uid, gid)), text(2, member.targetID)))
		if he != nil || ge != nil {
			return fmt.Errorf("key details HTTP=%v gRPC=%v", he, ge)
		}
		k, err = decode(first(g, 2))
		if err != nil || object(h["apiKey"])["id"] != member.targetID || string(first(k, 1)) != member.targetID {
			return fmt.Errorf("key details identity mismatch")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	authCtx := metadata.AppendToOutgoingContext(ctx, "x-buildbuddy-api-key", member.targetKey)
	var out []byte
	err := conn.Invoke(authCtx, "/auth.AuthService/Authenticate", []byte{}, &out, grpc.ForceCodec(wireCodec{}))
	if m.status == 1 {
		if err != nil {
			return err
		}
		f, err := decode(out)
		if err != nil {
			return err
		}
		parts := strings.Split(string(first(f, 1)), ".")
		if len(parts) != 3 {
			return fmt.Errorf("missing JWT")
		}
		b, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return err
		}
		var claims map[string]any
		if err = json.Unmarshal(b, &claims); err != nil {
			return err
		}
		eff := effectiveCaps(member.storedCaps, m.role)
		wantCaps := map[float64]bool{}
		if eff&4 != 0 {
			wantCaps[4] = true
		}
		if eff&1 != 0 {
			wantCaps[1] = true
		}
		gotCaps := map[float64]bool{}
		for _, v := range array(claims["capabilities"]) {
			n, ok := v.(float64)
			if !ok {
				return fmt.Errorf("invalid capability claim %v", v)
			}
			gotCaps[n] = true
		}
		if !reflect.DeepEqual(gotCaps, wantCaps) {
			return fmt.Errorf("Authenticate capabilities %v want %v", gotCaps, wantCaps)
		}
		if claims["group_id"] != gid || claims["user_id"] != member.uid {
			return fmt.Errorf("wrong auth claims %v", claims)
		}
	} else if status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
		return fmt.Errorf("removed key Authenticate: %v", err)
	}
	for i, c := range member.cacheClients {
		caps, ce := c.Capabilities.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
		if m.status == 1 {
			if ce != nil {
				return ce
			}
			wantWrite := effectiveCaps(member.storedCaps, m.role)&1 != 0
			if caps.GetCacheCapabilities().GetActionCacheUpdateCapabilities().GetUpdateEnabled() != wantWrite {
				return fmt.Errorf("cache %d advertised AC write capability did not converge", i)
			}
		} else if status.Code(ce) != codes.Unauthenticated && status.Code(ce) != codes.PermissionDenied {
			return fmt.Errorf("cache %d revoked capabilities: %v", i, ce)
		}
		// FindMissingBlobs with an empty valid request tests effective authentication
		// without changing cache contents or relying on cross-org CAS isolation.
		_, err := c.CAS.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{})
		if m.status == 1 {
			if err != nil {
				return fmt.Errorf("cache %d member authorization: %w", i, err)
			}
		} else if status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
			return fmt.Errorf("cache %d removed key authorization: %v", i, err)
		}
		if member.seed != nil {
			data, re := c.Read(ctx, "", member.seed)
			batch, be := c.CAS.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{Digests: []*repb.Digest{member.seed}})
			ac, ae := c.AC.GetActionResult(ctx, &repb.GetActionResultRequest{ActionDigest: member.seed})
			if m.status == 1 {
				if re != nil || string(data) != "membership surface seed" || be != nil || len(batch.GetResponses()) != 1 || batch.Responses[0].Status.GetCode() != 0 || string(batch.Responses[0].Data) != "membership surface seed" || ae != nil || ac.ExitCode != 17 {
					return fmt.Errorf("cache %d reads ByteStream=%v CAS=%v AC=%v", i, re, be, ae)
				}
			} else {
				for _, e := range []error{re, be, ae} {
					if status.Code(e) != codes.Unauthenticated && status.Code(e) != codes.PermissionDenied {
						return fmt.Errorf("cache %d revoked read: %v", i, e)
					}
				}
			}
		}

	}
	if member.seed != nil {
		rn := fmt.Sprintf("bytestream://app:1985/blobs/%s/%d", member.seed.Hash, member.seed.SizeBytes)
		// Public API exposes the same artifact as a server stream and byte range.
		stream, se := conn.NewStream(authCtx, &grpc.StreamDesc{ServerStreams: true}, "/api.v1.ApiService/GetFile", grpc.ForceCodec(wireCodec{}))
		var streamed []byte
		if se == nil {
			se = stream.SendMsg(text(1, rn))
			if se == nil {
				se = stream.CloseSend()
			}
			for se == nil {
				var chunk []byte
				se = stream.RecvMsg(&chunk)
				if se == nil {
					f, e := decode(chunk)
					if e != nil {
						return e
					}
					streamed = append(streamed, first(f, 1)...)
				}
			}
		}
		var rangeOut []byte
		re := conn.Invoke(authCtx, "/api.v1.ApiService/GetFileRange", join(text(1, rn), number(3, uint64(member.seed.SizeBytes))), &rangeOut, grpc.ForceCodec(wireCodec{}))
		if m.status == 1 {
			f, e := decode(rangeOut)
			if se != io.EOF || string(streamed) != "membership surface seed" || re != nil || e != nil || string(first(f, 1)) != "membership surface seed" {
				return fmt.Errorf("public API stream=%v range=%v", se, re)
			}
		} else {
			for _, e := range []error{se, re} {
				if status.Code(e) != codes.Unauthenticated && status.Code(e) != codes.PermissionDenied {
					return fmt.Errorf("revoked public API: %v", e)
				}
			}
		}

		req, e := http.NewRequestWithContext(ctx, "GET", member.fixtureHTTP+"/file/download?bytestream_url="+url.QueryEscape(rn), nil)
		if e != nil {
			return e
		}
		req.Header.Set("x-buildbuddy-api-key", member.targetKey)
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			return e
		}
		data, e := io.ReadAll(res.Body)
		res.Body.Close()
		if e != nil {
			return e
		}
		if m.status == 1 {
			if res.StatusCode != 200 || string(data) != "membership surface seed" {
				return fmt.Errorf("HTTP download %d %q", res.StatusCode, data)
			}
		} else if res.StatusCode == 200 {
			return fmt.Errorf("revoked HTTP download succeeded")
		}
		resource := join(bytesField(1, join(text(1, member.seed.Hash), number(2, uint64(member.seed.SizeBytes)))), number(4, 2), number(5, 1))
		meta, me := rpc(conn, member.targetKey, "GetCacheMetadata", join(bytesField(1, requestContext(member.uid, gid)), bytesField(2, resource)))
		if m.status == 1 {
			if me != nil || string(first(meta, 5)) != fmt.Sprint(member.seed.SizeBytes) {
				return fmt.Errorf("gRPC cache metadata: %v %v", meta, me)
			}
		} else if status.Code(me) != codes.Unauthenticated && status.Code(me) != codes.PermissionDenied {
			return fmt.Errorf("revoked metadata: %v", me)
		}
		hd, he := httpKeyRPC(ctx, member.fixtureHTTP, member.targetKey, "GetCacheMetadata", map[string]any{"requestContext": member.web.RequestContext(gid), "resourceName": map[string]any{"digest": map[string]any{"hash": member.seed.Hash, "sizeBytes": fmt.Sprint(member.seed.SizeBytes)}, "cacheType": "CAS", "digestFunction": "SHA256"}})
		if m.status == 1 {
			if he != nil || fmt.Sprint(hd["digestSizeBytes"]) != fmt.Sprint(member.seed.SizeBytes) {
				return fmt.Errorf("HTTP key metadata: %v %v", hd, he)
			}
		} else if he == nil || (!strings.Contains(he.Error(), "Unauthenticated") && !strings.Contains(he.Error(), "PermissionDenied")) {
			return fmt.Errorf("revoked HTTP metadata: %v", he)
		}

	}
	return nil
}

func httpKeyRPC(ctx context.Context, endpoint, key, method string, body map[string]any) (map[string]any, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/rpc/BuildBuddyService/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-buildbuddy-api-key", key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err = io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s HTTP %d: %s", method, res.StatusCode, b)
	}
	var out map[string]any
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
