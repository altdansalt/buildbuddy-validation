package userkeys

import (
	"context"
	"github.com/altdansalt/buildbuddy-validation/harness"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"reflect"
	"sort"
	"testing"
	"time"
)

// Wire field numbers are from released proto/{api_key,context}.proto and
// proto/api/v1/api_key.proto. No BuildBuddy generated dependency is needed.
type codec struct{}

func (codec) Name() string                    { return "proto" }
func (codec) Marshal(v any) ([]byte, error)   { return v.([]byte), nil }
func (codec) Unmarshal(b []byte, v any) error { *v.(*[]byte) = append([]byte(nil), b...); return nil }
func str(b []byte, n int, s string) []byte {
	return protowire.AppendString(protowire.AppendTag(b, protowire.Number(n), protowire.BytesType), s)
}
func num(b []byte, n int, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, protowire.Number(n), protowire.VarintType), v)
}

type fields map[int][][]byte

func parse(b []byte) fields {
	f := fields{}
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid response tag")
		}
		b = b[k:]
		var v []byte
		switch typ {
		case protowire.BytesType:
			v, k = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			var x uint64
			x, k = protowire.ConsumeVarint(b)
			v = protowire.AppendVarint(nil, x)
		default:
			k = protowire.ConsumeFieldValue(n, typ, b)
		}
		if k < 0 {
			panic("invalid response field")
		}
		f[int(n)] = append(f[int(n)], v)
		b = b[k:]
	}
	return f
}
func (f fields) one(n int) string {
	if len(f[n]) == 0 {
		return ""
	}
	return string(f[n][0])
}
func rpc(c *harness.Clients, method string, b []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []byte
	err := c.Conn.Invoke(ctx, method, b, &out, grpc.ForceCodec(codec{}))
	return out, err
}

const ui = "/buildbuddy.service.BuildBuddyService/"

type key struct {
	ID, Value, Label string
	CreatedAt        time.Time
	CreatedBy        string
	Caps             []int
	Visibility       []int
	Visible          bool
	Alive            bool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func fromWire(b []byte) key {
	f := parse(b)
	k := key{ID: f.one(1), Value: f.one(2), Label: f.one(3), Alive: true, Visible: f.one(5) == "\x01"}
	for _, v := range f[10] {
		for _, x := range v {
			k.Visibility = append(k.Visibility, int(x))
		}
	}
	for _, v := range f[4] {
		for _, x := range v {
			k.Caps = append(k.Caps, int(x))
		}
	}
	if md := f.one(9); md != "" {
		meta := parse([]byte(md))
		ts := parse([]byte(meta.one(1)))
		seconds, _ := protowire.ConsumeVarint([]byte(ts.one(1)))
		nanos, _ := protowire.ConsumeVarint([]byte(ts.one(2)))
		k.CreatedAt = time.Unix(int64(seconds), int64(nanos)).UTC()
		k.CreatedBy = meta.one(2)
	}
	sort.Ints(k.Caps)
	return k
}
func fromJSON(m map[string]any) key {
	k := key{Alive: true}
	k.ID, _ = m["id"].(string)
	k.Value, _ = m["value"].(string)
	k.Label, _ = m["label"].(string)
	k.Visible, _ = m["visibleToDevelopers"].(bool)
	if a, ok := m["visibility"].([]any); ok {
		for _, v := range a {
			if v == "VISIBLE_TO_GROUP_ADMINS" || v == float64(2) {
				k.Visibility = append(k.Visibility, 2)
			} else {
				k.Visibility = append(k.Visibility, -1)
			}
		}
	}
	if a, ok := m["capability"].([]any); ok {
		for _, v := range a {
			switch v {
			case "CACHE_WRITE":
				k.Caps = append(k.Caps, 1)
			case "CAS_WRITE":
				k.Caps = append(k.Caps, 4)
			default:
				if n, ok := v.(float64); ok {
					k.Caps = append(k.Caps, int(n))
				}
			}
		}
	}
	if md, ok := m["creationMetadata"].(map[string]any); ok {
		k.CreatedBy, _ = md["createdBy"].(string)
		if at, ok := md["createdAt"].(string); ok {
			k.CreatedAt, _ = time.Parse(time.RFC3339Nano, at)
		}
	}
	sort.Ints(k.Caps)
	return k
}
func compare(t *testing.T, want, got key, secret bool) {
	t.Helper()
	if got.Visible || !reflect.DeepEqual(got.Visibility, []int{2}) {
		t.Fatalf("unexpected user-key visibility: %v developer=%v", got.Visibility, got.Visible)
	}
	if got.ID != want.ID || got.Label != want.Label || !reflect.DeepEqual(got.Caps, want.Caps) {
		t.Fatalf("state mismatch: id=%s label=%q caps=%v; want id=%s label=%q caps=%v", got.ID, got.Label, got.Caps, want.ID, want.Label, want.Caps)
	}
	if secret && got.Value != want.Value {
		t.Fatal("single-key read changed secret")
	}
	if !secret && got.Value != "" {
		t.Fatal("list leaked secret")
	}
}
