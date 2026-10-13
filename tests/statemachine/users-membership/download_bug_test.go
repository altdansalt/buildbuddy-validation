package membership

import "testing"

// With the harness's non-default auth.enable_anonymous_usage=true, /file/download
// and unauthenticated gRPC reads return a blob that only an org key wrote; see
// findings/app-users-membership-anonymous-reads-org-blob.md. Fails until fixed.
func TestRemovedMemberHTTPDownload(t *testing.T) { runRemovedMemberHTTPDownload(t, false) }
