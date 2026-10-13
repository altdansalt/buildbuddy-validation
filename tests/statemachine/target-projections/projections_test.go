package projections_test

import (
	"context"
	"fmt"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/buildbuddy-validation/harness"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

// This codec transports the released schema directly, without building the app.
type wireCodec struct{}

func (wireCodec) Name() string                  { return "proto" }
func (wireCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (wireCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func readRPC(t *testing.T, f *harness.Fixture, path string, req []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []byte
	require.NoError(t, f.Clients(harness.App).Conn.Invoke(ctx, path, &req, &out, grpc.ForceCodec(wireCodec{})))
	return out
}
func values(b []byte, n protowire.Number) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid response tag")
		}
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				panic("invalid response bytes")
			}
			if tag == n {
				out = append(out, v)
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(tag, typ, b)
			if k < 0 {
				panic("invalid response value")
			}
			b = b[k:]
		}
	}
	return out
}

type targetModel struct {
	label, status         string
	attempts              int
	testStatus, apiStatus int64
}
type buildModel struct {
	id      string
	step    int
	now     int64
	targets []targetModel
	events  [][]byte
}

func build(t *testing.T, f *harness.Fixture, step int) buildModel {
	id := uuid.NewString()
	now := time.Now().UnixMilli()
	m := buildModel{id: id, step: step, now: now, targets: []targetModel{{"//pkg:alpha", "PASSED", 1, 1, 5}, {"//pkg:beta", "FLAKY", 2, 2, 11}}}
	if step == 1 {
		m.targets[0] = targetModel{"//pkg:alpha", "FAILED", 1, 4, 6}
		m.targets[1] = targetModel{"//pkg:beta", "PASSED", 1, 1, 5}
	}
	m.events = append(m.events, event(3, 5, nil, msg(str(1, id), num(2, now), str(3, "8.0.0"), str(4, "--color=no"), str(5, "test"))), event(15, 17, str(1, "cfg"), str(1, "k8-fastbuild")))
	metadata := []byte{}
	for _, kv := range [][2]string{{"REPO_URL", "https://example.invalid/projections"}, {"COMMIT_SHA", fmt.Sprintf("%040d", step+1)}, {"ROLE", "CI"}, {"BRANCH_NAME", "main"}} {
		metadata = append(metadata, field(1, msg(str(1, kv[0]), str(2, kv[1])))...)
	}
	m.events = append(m.events, event(24, 26, nil, metadata))
	var children []byte
	for _, x := range m.targets {
		children = append(children, field(2, field(16, str(1, x.label)))...)
	}
	m.events = append(m.events, msg(event(4, 6, str(1, "//pkg:all"), nil), children))
	for _, x := range m.targets {
		m.events = append(m.events, event(16, 18, str(1, x.label), msg(str(1, "sh_test rule"), num(2, 1))), msg(event(5, 8, msg(str(1, x.label), field(3, str(1, "cfg"))), num(1, 1)), field(2, field(6, msg(str(1, x.label+".out"), str(2, x.label), field(3, str(1, "cfg")))))), event(6, 7, msg(str(1, x.label+".out"), str(2, x.label), field(3, str(1, "cfg"))), msg(num(1, 1), str(8, "TestRunner"))))
		for attempt := 1; attempt <= x.attempts; attempt++ {
			content := artifactContent(id, x.label, attempt, x.testStatus == 4 || (x.attempts > 1 && attempt < x.attempts))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			d, err := f.Clients([]harness.Component{harness.App, harness.CacheProxy}[attempt%2]).Upload(ctx, "", content)
			cancel()
			require.NoError(t, err)
			uri := "bytestream://" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + harness.ResourceName("", d, false)
			artifact := msg(str(1, "test.xml"), str(2, uri))
			status := x.testStatus
			if x.attempts > 1 {
				status = 4
				if attempt == x.attempts {
					status = 1
				}
			}
			m.events = append(m.events, event(8, 10, msg(str(1, x.label), num(2, 1), num(4, int64(attempt)), field(5, str(1, "cfg"))), msg(field(2, artifact), num(5, status), str(9, fmt.Sprintf("%s/%s/%d", id, x.label, attempt)), num(6, now), num(3, 10))))
		}
		m.events = append(m.events, event(7, 9, msg(str(1, x.label), field(2, str(1, "cfg"))), msg(num(5, x.testStatus), num(1, int64(x.attempts)), num(10, 1), num(11, 1), num(15, int64(x.attempts)), num(7, now), num(8, now+10*int64(x.attempts)), num(9, 10*int64(x.attempts)))))
	}
	m.events = append(m.events, event(2, 3, num(1, 0), str(1, "log/"+id+"\n")))
	m.events = append(m.events, event(14, 16, nil, field(1, msg(str(1, "BUILD_USER"), str(2, "fixture-user")))))
	bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
	bd, err := f.Clients(harness.CacheProxy).Upload(bctx, "", bundleContent(id))
	bcancel()
	require.NoError(t, err)
	bundleURI := "bytestream://" + strings.TrimPrefix(f.Endpoint(harness.App).InternalGRPC, "grpc://") + "/" + harness.ResourceName("", bd, false)
	m.events = append(m.events, event(13, 15, str(1, "set-"+id), field(1, msg(str(1, "bundle.txt"), str(2, bundleURI), str(5, bd.Hash), num(6, bd.SizeBytes)))))
	m.events = append(m.events, event(20, 23, nil, field(1, msg(str(1, "report.txt"), field(3, []byte("report/"+id))))))
	exit := int64(0)
	name := "SUCCESS"
	if step == 1 {
		exit = 3
		name = "TESTS_FAILED"
	}
	m.events = append(m.events, msg(event(9, 14, nil, msg(field(3, msg(str(1, name), num(2, exit))), num(2, now+100))), num(20, 1)))
	for _, e := range m.events[1:] {
		m.events[0] = append(m.events[0], field(2, values(e, 1)[0])...)
	}
	return m
}
func check(t *testing.T, f *harness.Fixture, m buildModel, binary, dir string) {
	t.Helper()
	checkAdditional(t, f, m)
	checkLogsAndArtifacts(t, f, m, binary, dir)
	inv := rpc(t, f, "GetInvocation", map[string]any{"lookup": map[string]any{"invocationId": m.id}})
	invocation := objects(t, inv, "invocation")[0].(map[string]any)
	require.Equal(t, m.id, invocation["invocationId"])
	require.Equal(t, "COMPLETE_INVOCATION_STATUS", invocation["invocationStatus"])
	require.Equal(t, fmt.Sprint(len(m.targets)), fmt.Sprint(invocation["targetConfiguredCount"]))
	rawInvocation := readRPC(t, f, "/buildbuddy.service.BuildBuddyService/GetInvocation", field(2, str(1, m.id)))
	require.Len(t, values(rawInvocation, 2), 1)
	require.Equal(t, uint64(len(m.targets)), scalar(values(rawInvocation, 2)[0], 34))

	for _, x := range m.targets {
		found := false
		for _, v := range objects(t, invocation, "targetGroups") {
			g := v.(map[string]any)
			if g["status"] != x.status {
				continue
			}
			for _, v := range objects(t, g, "targets") {
				target := v.(map[string]any)
				if target["metadata"].(map[string]any)["label"] == x.label {
					require.Equal(t, x.status, target["status"])
					found = true
				}
			}
		}
		require.True(t, found, "invocation target group missing %s", x.label)
	}

	for _, x := range m.targets {
		output := runCLI(t, binary, dir, "view", m.id, x.label, "--test_filter=NoMatchingCase", "--target=grpc://"+f.Endpoint(harness.App).GRPC)
		require.NotContains(t, string(output), "artifact/")
		output = runCLI(t, binary, dir, "view", m.id, x.label, "--test_filter=TargetCase", "--target=grpc://"+f.Endpoint(harness.App).GRPC)
		if x.testStatus == 4 || x.attempts > 1 {
			require.Contains(t, string(output), "artifact/"+m.id+"/"+x.label+"/1")
		} else {
			require.NotContains(t, string(output), "artifact/")
		}

	}
	for _, x := range m.targets {
		ui := rpc(t, f, "GetTarget", map[string]any{"invocationId": m.id, "targetLabel": x.label})
		groups := objects(t, ui, "targetGroups")
		require.Len(t, groups, 1)
		targets := objects(t, groups[0].(map[string]any), "targets")
		require.Len(t, targets, 1)
		target := targets[0].(map[string]any)
		require.Equal(t, x.status, target["status"])
		results := objects(t, target, "testResultEvents")
		require.Len(t, results, x.attempts)
		summary := target["testSummary"].(map[string]any)
		require.Equal(t, x.status, summary["overallStatus"])
		require.Equal(t, float64(x.attempts), summary["attemptCount"])
		require.Len(t, objects(t, target, "actionEvents"), 1)
		for i, r := range results {
			e := r.(map[string]any)
			require.Equal(t, fmt.Sprintf("%s/%s/%d", m.id, x.label, i+1), e["testResult"].(map[string]any)["statusDetails"])
			file := objects(t, e["testResult"].(map[string]any), "testActionOutput")[0].(map[string]any)
			uri := file["uri"].(string)
			parts := strings.Split(uri, "/")
			size, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
			require.NoError(t, err)
			d := &repb.Digest{Hash: parts[len(parts)-2], SizeBytes: size}
			content := artifactContent(m.id, x.label, i+1, x.testStatus == 4 || (x.attempts > 1 && i+1 < x.attempts))
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				b, err := f.Clients(component).Read(ctx, "", d)
				cancel()
				require.NoError(t, err)
				require.Equal(t, content, b)
			}
			resp, err := f.Web.Client.Get(f.Web.URL + "/file/download?" + url.Values{"bytestream_url": {strings.Replace(uri, "bytestream://", "bytestream://"+f.APIKey+"@", 1)}, "filename": {"test.xml"}}.Encode())
			require.NoError(t, err)
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, resp.StatusCode, "%s", b)
			require.Equal(t, content, b)
			for _, component := range []harness.Component{harness.App, harness.CacheProxy} {
				downloaded := runCLI(t, binary, dir, "download", fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes), "--target=grpc://"+f.Endpoint(component).GRPC, "--api_key="+f.APIKey)
				require.Equal(t, content, downloaded)
			}

		}
		raw := readRPC(t, f, "/buildbuddy.service.BuildBuddyService/GetTarget", msg(str(1, m.id), str(2, x.label)))
		require.Len(t, values(raw, 1), 1)
		require.Len(t, values(values(raw, 1)[0], 2), 1)
		detail := values(values(raw, 1)[0], 2)[0]
		require.Len(t, values(detail, 6), x.attempts)
		require.Len(t, values(detail, 8), 1)
		require.Equal(t, uint64(x.apiStatus), scalar(detail, 2))
		require.Equal(t, []byte(x.label), values(values(detail, 1)[0], 2)[0])
		require.Equal(t, uint64(x.testStatus), scalar(values(detail, 7)[0], 5))

		code, rest := api(t, f, "GetTarget", map[string]any{"selector": map[string]any{"invocationId": m.id, "label": x.label}})
		require.Equal(t, 200, code, "%v", rest)
		rt := objects(t, rest, "target")
		require.Len(t, rt, 1)
		require.Equal(t, x.status, rt[0].(map[string]any)["status"])
		gr := readRPC(t, f, "/api.v1.ApiService/GetTarget", field(1, msg(str(1, m.id), str(4, x.label))))
		require.Len(t, values(gr, 1), 1)
		require.Equal(t, []byte(x.label), values(values(gr, 1)[0], 2)[0])
		require.Equal(t, uint64(x.apiStatus), scalar(values(gr, 1)[0], 3))
		code, actions := api(t, f, "GetAction", map[string]any{"selector": map[string]any{"invocationId": m.id, "targetLabel": x.label}})
		require.Equal(t, 200, code, "%v", actions)
		ar := objects(t, actions, "action")
		require.Len(t, ar, x.attempts+2)
		for _, a := range ar {
			require.Equal(t, x.label, a.(map[string]any)["targetLabel"])
			require.Equal(t, m.id, a.(map[string]any)["id"].(map[string]any)["invocationId"])
			if a.(map[string]any)["attempt"] != nil {
				require.Len(t, objects(t, a.(map[string]any), "file"), 1)
				attempt, err := strconv.Atoi(fmt.Sprint(a.(map[string]any)["attempt"]))
				require.NoError(t, err)
				require.GreaterOrEqual(t, attempt, 1)
				require.LessOrEqual(t, attempt, x.attempts)
				file := objects(t, a.(map[string]any), "file")[0].(map[string]any)
				uiFile := objects(t, results[attempt-1].(map[string]any)["testResult"].(map[string]any), "testActionOutput")[0].(map[string]any)
				require.Equal(t, uiFile["uri"], file["uri"])

			}
		}
		ga := readRPC(t, f, "/api.v1.ApiService/GetAction", field(1, msg(str(1, m.id), str(5, x.label))))
		require.Len(t, values(ga, 1), len(ar))
		attempts := map[uint64]bool{}
		for _, a := range values(ga, 1) {
			require.Equal(t, []byte(x.label), values(a, 3)[0])
			identity := values(a, 1)[0]
			require.Equal(t, []byte(m.id), values(identity, 1)[0])
			require.Equal(t, []byte("cfg"), values(identity, 3)[0])
			if attempt := scalar(a, 6); attempt > 0 {
				require.False(t, attempts[attempt])
				attempts[attempt] = true
				require.Equal(t, uint64(1), scalar(a, 5))
				require.Len(t, values(a, 2), 1)
			}
		}
		require.Len(t, attempts, x.attempts)

	}
}
func TestSuccessiveBuildProjections(t *testing.T) {
	f := harness.Start(t, harness.Options{Components: []harness.Component{harness.App, harness.CacheProxy}, Args: map[harness.Component][]string{harness.App: {"--app.enable_target_tracking=true"}}})
	for _, method := range []string{"GetTargetStats", "GetDailyTargetStats", "GetTargetFlakeSamples", "GetTargetTrends"} {
		_, err := f.Web.RPC(method, map[string]any{"requestContext": f.Web.RequestContext(f.GroupID)})
		require.Error(t, err, "OLAP reader %s unexpectedly available", method)
		require.Contains(t, err.Error(), "Unimplemented")
	}
	binary, dir := releasedCLI(t, f)
	var model []buildModel
	for step := 0; step < 3; step++ {
		m := build(t, f, step)
		publish(t, f, m.id, m.events)
		model = append(model, m)
		for _, old := range model {
			check(t, f, old, binary, dir)
		}
		checkHistory(t, f, model)
		checkSearch(t, f, model)
		publish(t, f, m.id, m.events) // A valid retry must leave projections unchanged.
		for _, old := range model {
			check(t, f, old, binary, dir)
		}
		checkHistory(t, f, model)
		checkSearch(t, f, model)
	}
}

func checkHistory(t *testing.T, f *harness.Fixture, model []buildModel) {
	t.Helper()
	expected := map[string]map[string]string{}
	for _, b := range model {
		for _, x := range b.targets {
			if expected[x.label] == nil {
				expected[x.label] = map[string]string{}
			}
			expected[x.label][b.id] = x.status
		}
	}
	var got map[string]map[string]string
	require.Eventually(t, func() bool {
		res, err := f.Web.RPC("GetTargetHistory", map[string]any{"requestContext": f.Web.RequestContext(f.GroupID), "query": map[string]any{"repoUrl": "https://example.invalid/projections"}, "serverSidePagination": true})
		if err != nil {
			return false
		}
		got = map[string]map[string]string{}
		for _, v := range objects(t, res, "invocationTargets") {
			h := v.(map[string]any)
			label := h["target"].(map[string]any)["label"].(string)
			got[label] = map[string]string{}
			for _, v := range objects(t, h, "targetStatus") {
				s := v.(map[string]any)
				got[label][s["invocationId"].(string)] = s["status"].(string)
			}
		}
		return fmt.Sprint(got) == fmt.Sprint(expected)
	}, 30*time.Second, 100*time.Millisecond, "history got %v want %v", got, expected)
	// gRPC shares the same repository query, with explicit fixture group context.
	out := readRPC(t, f, "/buildbuddy.service.BuildBuddyService/GetTargetHistory", msg(field(1, str(2, f.GroupID)), field(2, str(3, "https://example.invalid/projections")), num(5, 1)))
	require.Len(t, values(out, 2), 2)
	for _, h := range values(out, 2) {
		require.Len(t, values(h, 3), len(model))
		label := string(values(values(h, 1)[0], 2)[0])
		require.Contains(t, expected, label)
		statuses := map[string]uint64{}
		for _, v := range values(h, 3) {
			id := string(values(v, 1)[0])
			require.NotContains(t, statuses, id)
			statuses[id] = scalar(v, 3)
		}
		for _, b := range model {
			for _, x := range b.targets {
				if x.label == label {
					require.Equal(t, uint64(x.apiStatus), statuses[b.id])
				}
			}
		}

	}
}

func bundleContent(id string) []byte { return []byte("bundle/" + id) }

func artifactContent(id, label string, attempt int, failed bool) []byte {
	outcome := ""
	if failed {
		outcome = fmt.Sprintf("<failure message=\"failure\">artifact/%s/%s/%d</failure>", id, label, attempt)
	}
	return []byte("<testsuites><testsuite name=\"fixture\"><testcase name=\"TargetCase\">" + outcome + "</testcase></testsuite></testsuites>")
}

func scalar(b []byte, n protowire.Number) uint64 {
	for len(b) > 0 {
		tag, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			panic("invalid tag")
		}
		b = b[k:]
		if typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				panic("invalid varint")
			}
			if tag == n {
				return v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(tag, typ, b)
			if k < 0 {
				panic("invalid field")
			}
			b = b[k:]
		}
	}
	return 0
}
