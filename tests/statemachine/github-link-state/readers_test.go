package githubstate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"
)

func (d *driver) context() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}
func authDenial(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "missing user access token") || strings.Contains(s, "github account link is required") || strings.Contains(s, "no linked GitHub account") || strings.Contains(s, "GitHub account is not linked") || strings.Contains(s, "The selected group does not have a GitHub account linked")
}
func (d *driver) jsonRPC(method string, req map[string]any) (map[string]any, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(d.context(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", d.f.Web.URL+"/rpc/BuildBuddyService/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := d.f.Web.Client.Do(r)
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
	err = json.Unmarshal(b, &out)
	return out, err
}

// Every reader gets valid arguments, and exact model-backed data in all three
// representations. Local org mutations must not revoke the user's OAuth token.
func (d *driver) contentReaders() error {
	base := join(str(2, "fixture-owner"), str(3, "project"))
	type probe struct {
		method    string
		extra     []byte
		args      map[string]any
		field     int
		value     string
		jsonField string
		jsonValue any
	}
	probes := []probe{
		{"GetGithubContent", str(4, "README.md"), map[string]any{"path": "README.md"}, 2, "fixture content\n", "content", "Zml4dHVyZSBjb250ZW50Cg=="},
		{"GetGithubBlob", str(4, "abc"), map[string]any{"sha": "abc"}, 2, "fixture content\n", "content", "Zml4dHVyZSBjb250ZW50Cg=="},
		{"GetGithubTree", str(4, "main"), map[string]any{"ref": "main"}, 2, "tree-sha", "sha", "tree-sha"},
		{"GetGithubCompare", join(str(4, "main"), str(5, "topic")), map[string]any{"base": "main", "head": "topic"}, 2, string([]byte{1}), "aheadBy", "1"},
		{"GetGithubForks", nil, nil, 2, string(str(1, "fixture-fork")), "forks", []any{map[string]any{"owner": "fixture-fork"}}},
		{"GetGithubCommits", str(4, "main"), map[string]any{"sha": "main"}, 2, string(join(str(1, "commit-sha"), str(2, "tree-sha"), str(3, "fixture commit"))), "commits", []any{map[string]any{"sha": "commit-sha", "treeSha": "tree-sha", "message": "fixture commit"}}},
		{"GetGithubPullRequest", nil, nil, 0, "", "", nil},
		{"GetGithubPullRequestDetails", num(4, 1), map[string]any{"pull": "1"}, 19, "PR_fixture", "pullId", "PR_fixture"},
	}
	for _, p := range probes {
		extra := join(base, p.extra)
		if p.method == "GetGithubPullRequest" {
			extra = nil
		}
		req := map[string]any{"requestContext": d.f.Web.RequestContext(d.f.GroupID), "owner": "fixture-owner", "repo": "project"}
		for k, v := range p.args {
			req[k] = v
		}
		if p.method == "GetGithubPullRequest" {
			delete(req, "owner")
			delete(req, "repo")
		}
		if !d.m.user {
			_, g := d.grpc(p.method, extra)
			_, h := d.http(p.method, extra)
			_, j := d.jsonRPC(p.method, req)
			for _, e := range []error{g, h, j} {
				if !authDenial(e) {
					return fmt.Errorf("%s expected token denial: %v", p.method, e)
				}
			}
			continue
		}
		b, err := d.read(p.method, extra)
		if err != nil {
			return err
		}
		if p.field != 0 && string(first(b, protowire.Number(p.field))) != p.value {
			return fmt.Errorf("%s model mismatch: %x", p.method, b)
		}
		j, err := d.jsonRPC(p.method, req)
		if err != nil {
			return err
		}
		if p.jsonField != "" && !reflect.DeepEqual(j[p.jsonField], p.jsonValue) {
			return fmt.Errorf("%s JSON model mismatch: %v", p.method, j)
		}
		if p.method == "GetGithubPullRequest" {
			if len(fields(b, 3))+len(fields(b, 4))+len(fields(b, 5)) != 0 || len(j) > 0 {
				return fmt.Errorf("PR dashboard not empty: %x %v", b, j)
			}
		}
	}
	return nil
}

func (d *driver) workflowReaders() error {
	b, err := d.read("GetWorkflows", nil)
	if err != nil {
		return err
	}
	if len(fields(b, 2)) != 0 {
		return fmt.Errorf("unexpected legacy workflow: %x", b)
	}
	j, err := d.jsonRPC("GetWorkflows", map[string]any{"requestContext": d.f.Web.RequestContext(d.f.GroupID)})
	if err != nil {
		return err
	}
	if rows, ok := j["workflow"].([]any); ok && len(rows) > 0 {
		return fmt.Errorf("unexpected JSON workflow: %v", j)
	}
	// No public RPC creates legacy workflows in this release. History requires
	// OLAP even for zero repos; assert its precise prerequisite on every step.
	_, g := d.grpc("GetWorkflowHistory", nil)
	_, h := d.http("GetWorkflowHistory", nil)
	_, e := d.jsonRPC("GetWorkflowHistory", map[string]any{"requestContext": d.f.Web.RequestContext(d.f.GroupID)})
	for _, err := range []error{g, h, e} {
		if err == nil || !strings.Contains(err.Error(), "database not configured") {
			return fmt.Errorf("workflow history prerequisite: %v", err)
		}
	}
	return nil
}
func (d *driver) keyReaders() error {
	for i, c := range d.keys {
		for _, method := range []string{"GetLinkedGitHubRepos", "GetWorkflows"} {
			req := d.request(method, nil)
			var b []byte
			ctx, cancel := context.WithTimeout(d.context(), 5*time.Second)
			err := c.Invoke(ctx, "/buildbuddy.service.BuildBuddyService/"+method, &req, &b, grpc.ForceCodec(wireCodec{}))
			cancel()
			if err != nil {
				return fmt.Errorf("key %d %s: %w", i, method, err)
			}
			want, err := d.grpc(method, nil)
			if err != nil {
				return err
			}
			if !bytes.Equal(b, want) {
				return fmt.Errorf("key %d %s differs: %x vs %x", i, method, b, want)
			}
		}
	}
	return nil
}
