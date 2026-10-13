package orgkeys

import (
	"context"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

// Raw-wire bindings to proto/api_key.proto (v2.314.0) avoid building product source.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}

func bytesField(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}

func varintField(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}

// parse returns the repeated length-delimited and varint fields of one message.
func parse(t *testing.T, b []byte) (bytes, varints map[protowire.Number][][]byte) {
	t.Helper()
	bytes, varints = map[protowire.Number][][]byte{}, map[protowire.Number][][]byte{}
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			t.Fatal("bad tag")
		}
		b = b[k:]
		switch typ {
		case protowire.BytesType:
			v, k2 := protowire.ConsumeBytes(b)
			bytes[n], k = append(bytes[n], v), k2
		case protowire.VarintType:
			v, k2 := protowire.ConsumeVarint(b)
			varints[n], k = append(varints[n], protowire.AppendVarint(nil, v)), k2
		default:
			k = protowire.ConsumeFieldValue(n, typ, b)
		}
		if k < 0 {
			t.Fatal("bad field")
		}
		b = b[k:]
	}
	return
}

// visibilityMask ORs ApiKey.visibility (field 10, packed or unpacked).
func visibilityMask(t *testing.T, apiKey []byte) uint64 {
	_, v := parse(t, apiKey)
	b, _ := parse(t, apiKey)
	var mask uint64
	for _, e := range append(b[10], v[10]...) {
		for len(e) > 0 {
			x, n := protowire.ConsumeVarint(e)
			mask |= x
			e = e[n:]
		}
	}
	return mask
}

// createAndRead creates a key with createFields, then reports the visibility
// mask observed through CreateApiKey, GetApiKey and GetApiKeys.
func createAndRead(t *testing.T, createFields []byte) map[string]uint64 {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := harness.Dial(t, f.Endpoint(harness.App).GRPC, f.Web.CreateAPIKey(t, f.GroupID, "visibility-admin", []string{"ORG_ADMIN"}))
	call := func(method string, req []byte) []byte {
		var out []byte
		if err := admin.Conn.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &out, grpc.ForceCodec(wireCodec{})); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return out
	}
	rc := bytesField(1, bytesField(2, []byte(f.GroupID))) // request_context.group_id
	req := append(append(append([]byte(nil), rc...), bytesField(3, []byte("vis"))...), varintField(4, 1)...)
	created, _ := parse(t, call("CreateApiKey", append(req, createFields...)))
	id := string(func() []byte { b, _ := parse(t, created[2][0]); return b[1][0] }())

	got := map[string]uint64{"CreateApiKey": visibilityMask(t, created[2][0])}
	one, _ := parse(t, call("GetApiKey", append(append([]byte(nil), rc...), bytesField(2, []byte(id))...)))
	got["GetApiKey"] = visibilityMask(t, one[2][0])
	list, _ := parse(t, call("GetApiKeys", rc))
	for _, k := range list[2] {
		if b, _ := parse(t, k); string(b[1][0]) == id {
			got["GetApiKeys"] = visibilityMask(t, k)
		}
	}
	return got
}

const wantMask = 3 // VISIBLE_TO_DEVELOPERS(1) | VISIBLE_TO_GROUP_ADMINS(2)

// Control: the deprecated visible_to_developers bool (field 5) is honored on every surface.
func TestVisibilityDeprecatedBool(t *testing.T) {
	for surface, mask := range createAndRead(t, varintField(5, 1)) {
		if mask != wantMask {
			t.Errorf("%s visibility mask=%d want %d", surface, mask, wantMask)
		}
	}
}

// Divergence: the documented replacement, repeated visibility (field 8) set to
// [VISIBLE_TO_DEVELOPERS, VISIBLE_TO_GROUP_ADMINS], is ignored; mask is 2.
func TestVisibilityRepeatedField(t *testing.T) {
	req := append(varintField(8, 1), varintField(8, 2)...)
	for surface, mask := range createAndRead(t, req) {
		if mask != wantMask {
			t.Errorf("%s visibility mask=%d want %d", surface, mask, wantMask)
		}
	}
}
