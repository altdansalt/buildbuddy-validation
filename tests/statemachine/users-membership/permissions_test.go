package membership

import (
	"fmt"
	"reflect"
	"sort"
)

// Released v2.314.0 capability-filter permission inventory. Compare the entire
// advertised list, including API aliases and duplicate method basenames.
var unfilteredRPCs = []string{"GetUser", "CreateUser", "GetGroup", "GetInvocation", "GetEventLogChunk", "GetEventLog", "GetCacheScoreCard", "GetCacheMetadata", "GetTree", "GetTreeDirectorySizes", "GetTarget", "GetTargetHistory", "GetExecution", "GetExecutionDownloads", "WaitExecution", "GetZipManifest", "JoinGroup", "GetBazelConfig", "GetInvocation", "GetAuditLog", "GetLog", "DeleteFile", "GetTarget", "GetAction", "GetFile", "GetFileRange", "DeleteFile", "UnlinkUserGitHubAccount", "GetGithubUserInstallations", "GetGithubUser", "GetGithubRepo", "GetGithubContent", "GetGithubTree", "CreateGithubTree", "GetGithubBlob", "CreateGithubBlob", "CreateGithubPull", "MergeGithubPull", "GetGithubCompare", "GetGithubForks", "CreateGithubFork", "GetGithubCommits", "CreateGithubCommit", "UpdateGithubRef", "CreateGithubRef", "GetGithubPullRequest", "GetGithubPullRequestDetails", "CreateGithubPullRequestComment", "UpdateGithubPullRequestComment", "DeleteGithubPullRequestComment", "SendGithubPullRequestReview", "GetAuditLogs", "SendNotification", "CreateGroup"}
var groupMemberRPCs = []string{"SearchInvocation", "GetInvocationStat", "GetTrend", "GetStatHeatmap", "GetStatDrilldown", "GetTargetTrends", "SearchExecution", "GetExecutionTimeline", "GetTargetStats", "GetDailyTargetStats", "GetTargetFlakeSamples", "GetInvocationFilterSuggestions", "GetWorkflows", "GetRepos", "GetWorkflowHistory", "GetLinkedGitHubRepos", "UpdateInvocation", "DeleteInvocation", "CancelExecutions", "ExecuteWorkflow", "ExecuteWorkflow", "InvalidateSnapshot", "WriteEventLog", "UpdateRunStatus", "GetApiKeys", "GetApiKey", "GetUserApiKeys", "GetUserApiKey", "CreateUserApiKey", "CreateUserApiKey", "UpdateUserApiKey", "DeleteUserApiKey", "Run", "Run", "Search", "KytheProxy", "Index", "RepoStatus", "GetWorkspace", "SaveWorkspace", "GetWorkspaceDirectory", "GetWorkspaceFile"}
var groupAdminOnlyRPCs = []string{"UpdateGroup", "GetGroupUsers", "UpdateGroupUsers", "GetUserLists", "GetUserList", "CreateUserList", "DeleteUserList", "UpdateUserList", "UpdateUserListMembership", "UnlinkGitHubAccount", "LinkGitHubAppInstallation", "GetGitHubAppInstallations", "UnlinkGitHubAppInstallation", "UpdateGitHubAppInstallation", "GetAccessibleGitHubRepos", "GetGitHubAppInstallPath", "LinkGitHubRepo", "UnlinkGitHubRepo", "UpdateGitHubRepoSettings", "CreateApiKey", "UpdateApiKey", "DeleteApiKey", "GetPublicKey", "ListSecrets", "UpdateSecret", "DeleteSecret", "DeleteWorkflow", "InvalidateAllSnapshotsForRepo", "GetExecutionNodes", "GetCacheProxies", "ListCacheProxies", "GetCacheProxy", "GetUsage", "GetUsageAlertingRules", "CreateUsageAlertingRule", "DeleteUsageAlertingRule", "GetCurrentBill", "GetSSOConfig", "SetSSOConfig", "GetEncryptionConfig", "SetEncryptionConfig", "CreateRepo", "GetIPRules", "AddIPRule", "UpdateIPRule", "DeleteIPRule", "GetIPRulesConfig", "SetIPRulesConfig", "GetGCPProject"}

func comparePermissions(got []string, mode int) error {
	want := append([]string(nil), unfilteredRPCs...)
	if mode >= 0 {
		want = append(want, groupMemberRPCs...)
	}
	if mode == 1 {
		want = append(want, groupAdminOnlyRPCs...)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("allowedRPC got %v want %v", got, want)
	}
	return nil
}
