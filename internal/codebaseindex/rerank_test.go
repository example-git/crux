package codebaseindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRerankSearchResultsDeduplicatesSymbolsAndExpandsCompleteFunction(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "internal", "shell", "background.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	source := `package shell

type BackgroundShellManager struct{}

func (m *BackgroundShellManager) recover() error {
	records := loadRecords()
	for _, record := range records {
		if record.Status == "running" {
			record.Status = "lost"
		}
	}
	return saveRecords(records)
}

func loadRecords() []record { return nil }
func saveRecords([]record) error { return nil }
type record struct { Status string }
`
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	results := []SearchResult{
		{Chunk: Chunk{Path: "internal/shell/background.go", ChunkIndex: 0, StartLine: 5, EndLine: 9, Content: "func recover running lost"}, Score: 0.91},
		{Chunk: Chunk{Path: "internal/shell/background.go", ChunkIndex: 1, StartLine: 8, EndLine: 13, Content: "running status lost restart"}, Score: 0.89},
		{Chunk: Chunk{Path: "internal/shell/background.go", ChunkIndex: 2, StartLine: 15, EndLine: 15, Content: "loadRecords persistence"}, Score: 0.72},
	}

	reranked := RerankSearchResults(root, "background shell recovery restart running to lost", results, 10)

	require.Len(t, reranked, 2)
	require.Equal(t, "BackgroundShellManager.recover", reranked[0].Symbol)
	require.Equal(t, SearchRoleDirect, reranked[0].Role)
	require.Equal(t, 5, reranked[0].Chunk.StartLine)
	require.Equal(t, 13, reranked[0].Chunk.EndLine)
	require.Contains(t, reranked[0].Chunk.Content, "func (m *BackgroundShellManager) recover() error")
	require.Contains(t, reranked[0].Chunk.Content, `record.Status = "lost"`)
}

func TestRerankSearchResultsBoostsFacetCoverageAndDiversifiesRoles(t *testing.T) {
	root := t.TempDir()
	results := []SearchResult{
		{Chunk: Chunk{Path: "internal/task/cleanup.go", StartLine: 1, EndLine: 5, Content: "remove completed retained task"}, Score: 0.90},
		{Chunk: Chunk{Path: "internal/shell/recovery.go", StartLine: 10, EndLine: 20, Content: "background shell process restart recovers running nonterminal work as lost"}, Score: 0.84},
		{Chunk: Chunk{Path: "internal/shell/recovery_test.go", StartLine: 30, EndLine: 40, Content: "test background shell restart running becomes lost"}, Score: 0.82},
		{Chunk: Chunk{Path: "docs/background-tasks.md", StartLine: 50, EndLine: 60, Content: "restart contract running local work becomes lost"}, Score: 0.80},
		{Chunk: Chunk{Path: "internal/task/metadata.go", StartLine: 70, EndLine: 80, Content: "persist task status running lost"}, Score: 0.81},
	}

	reranked := RerankSearchResults(root, "background shell recovery process restart running work lost", results, 5)

	require.Len(t, reranked, 5)
	require.Equal(t, "internal/shell/recovery.go", reranked[0].Chunk.Path)
	require.Equal(t, SearchRoleDirect, reranked[0].Role)
	roles := make(map[SearchRole]bool)
	for _, result := range reranked {
		roles[result.Role] = true
	}
	require.True(t, roles[SearchRoleValidation])
	require.True(t, roles[SearchRolePersistence])
	require.True(t, roles[SearchRoleContract])
	require.Contains(t, reranked[0].Explanation, "Matches ")
}

func TestRerankSearchResultsPrefersPreciseSupportingEvidence(t *testing.T) {
	root := t.TempDir()
	results := []SearchResult{
		{
			Chunk:  Chunk{Path: "internal/agent/background_agent.go", StartLine: 120, EndLine: 173, Content: "background agent recovery after process restart converts running work to lost"},
			Score:  0.60,
			Symbol: "BackgroundAgentManager.recover",
		},
		{
			Chunk:  Chunk{Path: "internal/agent/tools/bash.go", StartLine: 199, EndLine: 426, Content: "background running process command"},
			Score:  0.60,
			Symbol: "BashTool.Run",
		},
		{
			Chunk:  Chunk{Path: "internal/shell/background.go", StartLine: 147, EndLine: 201, Content: "background shell recovery after process restart converts running nonterminal work to lost"},
			Score:  0.59,
			Symbol: "BackgroundShellManager.recover",
		},
		{
			Chunk:  Chunk{Path: "internal/backend/tasks.go", StartLine: 1, EndLine: 50, Content: "list background tasks and return running status"},
			Score:  0.59,
			Symbol: "Backend.Tasks",
		},
		{
			Chunk:  Chunk{Path: "internal/shell/background_test.go", StartLine: 507, EndLine: 541, Content: "persisted running background shell recovers as lost after restart"},
			Score:  0.58,
			Symbol: "TestBackgroundShellManagerRecoveryMarksActiveLost",
		},
		{
			Chunk:  Chunk{Path: "internal/app/app.go", StartLine: 107, EndLine: 236, Content: "construct background shell manager and agent manager during application startup"},
			Score:  0.57,
			Symbol: "New",
		},
		{
			Chunk:  Chunk{Path: "internal/app/app.go", StartLine: 854, EndLine: 898, Content: "graceful shutdown calls KillAll so running background shells become killed rather than restart lost"},
			Score:  0.56,
			Symbol: "App.shutdown",
		},
		{
			Chunk:  Chunk{Path: "internal/agent/background_agent_test.go", StartLine: 230, EndLine: 270, Content: "persisted running background agent recovery marks active task lost after restart"},
			Score:  0.55,
			Symbol: "TestBackgroundAgentManagerRecoveryMarksActiveLost",
		},
		{
			Chunk:  Chunk{Path: "internal/task/metadata.go", StartLine: 70, EndLine: 110, Content: "serialize and persist running task status for restart recovery"},
			Score:  0.54,
			Symbol: "StateToRecord",
		},
		{
			Chunk: Chunk{Path: "docs/background-tasks.md", StartLine: 300, EndLine: 309, Content: "persistence restart contract: persisted pending or running local tasks become lost on workspace startup"},
			Score: 0.50,
		},
		{
			Chunk: Chunk{Path: "internal/agent/tools/job_list.md", StartLine: 1, EndLine: 3, Content: "list background shell jobs"},
			Score: 0.57,
		},
	}

	reranked := RerankSearchResults(root, "background shell recovery after process restart running nonterminal work transitions to lost", results, 8)

	require.Len(t, reranked, 8)
	require.Equal(t, "internal/shell/background.go", reranked[0].Chunk.Path)
	require.Equal(t, SearchRoleDirect, reranked[0].Role)

	byPath := make(map[string][]SearchResult)
	bySymbol := make(map[string]SearchResult)
	for _, result := range reranked {
		byPath[result.Chunk.Path] = append(byPath[result.Chunk.Path], result)
		bySymbol[result.Symbol] = result
	}
	require.Equal(t, SearchRoleStartup, bySymbol["New"].Role)
	require.Equal(t, SearchRoleComparison, bySymbol["App.shutdown"].Role)
	require.Equal(t, SearchRoleValidation, bySymbol["TestBackgroundShellManagerRecoveryMarksActiveLost"].Role)
	require.Equal(t, SearchRoleContract, byPath["docs/background-tasks.md"][0].Role)
	require.Equal(t, SearchRoleParallel, bySymbol["BackgroundAgentManager.recover"].Role)
	require.Equal(t, SearchRoleValidation, bySymbol["TestBackgroundAgentManagerRecoveryMarksActiveLost"].Role)
	require.Equal(t, SearchRolePersistence, bySymbol["StateToRecord"].Role)
	require.NotContains(t, byPath, "internal/agent/tools/bash.go")
	require.NotContains(t, byPath, "internal/backend/tasks.go")
	require.NotContains(t, byPath, "internal/agent/tools/job_list.md")
}

func TestInferSearchRoleTreatsToolMarkdownAsRelated(t *testing.T) {
	direct := SearchResult{Symbol: "BackgroundShellManager.recover"}
	result := SearchResult{
		Chunk: Chunk{Path: "internal/agent/tools/job_list.md", Content: "List background shell jobs."},
	}

	require.Equal(t, SearchRoleRelated, inferSearchRole(result, direct, significantSearchTerms("background shell recovery")))
}

func TestRerankSearchResultsPrefersEvidenceChainAcrossDomains(t *testing.T) {
	results := []SearchResult{
		{Chunk: Chunk{Path: "pkg/migrate/apply.go", StartLine: 20, EndLine: 60, Content: "apply database schema migration and record the new version"}, Score: 0.72, Symbol: "Runner.Apply"},
		{Chunk: Chunk{Path: "pkg/api/migrations.go", StartLine: 8, EndLine: 10, Content: "func (s *Server) Migrations() View { return s.listMigrations() }"}, Score: 0.68, Symbol: "Server.Migrations"},
		{Chunk: Chunk{Path: "pkg/migrate/apply_test.go", StartLine: 30, EndLine: 65, Content: "test database schema migration applies version and rollback restores prior version"}, Score: 0.61, Symbol: "TestRunnerApplyAndRollback"},
		{Chunk: Chunk{Path: "docs/database-migrations.md", StartLine: 40, EndLine: 52, Content: "migration contract requires recording each schema version before startup continues and defines rollback"}, Score: 0.52},
		{Chunk: Chunk{Path: "pkg/migrate/records.go", StartLine: 12, EndLine: 35, Content: "persist migration version record before applying database schema changes"}, Score: 0.55, Symbol: "VersionRecord.Save"},
		{Chunk: Chunk{Path: "pkg/app/start.go", StartLine: 15, EndLine: 32, Content: "initialize database and call runner apply during application startup"}, Score: 0.57, Symbol: "Initialize"},
	}

	reranked := RerankSearchResults(t.TempDir(), "database schema migration startup version persistence rollback", results, 5)

	require.Len(t, reranked, 5)
	require.Equal(t, "pkg/migrate/apply.go", reranked[0].Chunk.Path)
	paths := make(map[string]SearchRole)
	for _, result := range reranked {
		paths[result.Chunk.Path] = result.Role
	}
	require.Equal(t, SearchRoleStartup, paths["pkg/app/start.go"])
	require.Equal(t, SearchRoleValidation, paths["pkg/migrate/apply_test.go"])
	require.Equal(t, SearchRoleContract, paths["docs/database-migrations.md"])
	require.Equal(t, SearchRolePersistence, paths["pkg/migrate/records.go"])
	require.NotContains(t, paths, "pkg/api/migrations.go")
}

func TestLowInformationExcerptPenaltyPreservesConciseExactMatches(t *testing.T) {
	exact := SearchResult{Chunk: Chunk{Content: "func CurrentTheme() Theme { return selectedTheme }", StartLine: 1, EndLine: 1}}
	weak := SearchResult{Chunk: Chunk{Content: "return service.listItems()", StartLine: 1, EndLine: 1}}

	require.Zero(t, lowInformationExcerptPenalty(exact, 1))
	require.Greater(t, lowInformationExcerptPenalty(weak, 0.2), 0.1)
}

func TestPrimaryEvidencePenaltyHonorsExplicitEvidenceQueries(t *testing.T) {
	testResult := SearchResult{Chunk: Chunk{Path: "pkg/service_test.go"}}
	docResult := SearchResult{Chunk: Chunk{Path: "docs/service.md"}}

	require.Zero(t, primaryEvidencePenalty(testResult, significantSearchTerms("find validation tests")))
	require.Zero(t, primaryEvidencePenalty(docResult, significantSearchTerms("find contract documentation")))
	require.Greater(t, primaryEvidencePenalty(testResult, significantSearchTerms("service implementation")), 0.0)
	require.Greater(t, primaryEvidencePenalty(docResult, significantSearchTerms("service implementation")), 0.0)
}

func TestRerankSearchResultsPromotesSharedServiceCallee(t *testing.T) {
	results := []SearchResult{
		{Chunk: Chunk{Path: "tools/view.go", StartLine: 1, EndLine: 35, Content: "view reads workspace file after permissions.Request(ctx, operation) approval"}, Score: 0.81, Symbol: "View.Run"},
		{Chunk: Chunk{Path: "tools/download.go", StartLine: 1, EndLine: 35, Content: "download writes workspace file after permissions.Request(ctx, operation) approval"}, Score: 0.78, Symbol: "Download.Run"},
		{Chunk: Chunk{Path: "tools/fetch.go", StartLine: 1, EndLine: 35, Content: "fetch writes workspace result after permissions.Request(ctx, operation) approval"}, Score: 0.76, Symbol: "Fetch.Run"},
		{Chunk: Chunk{Path: "permission/service.go", StartLine: 40, EndLine: 110, Content: "central permission request enforces workspace read write policy hooks approvals and denials for every tool"}, Score: 0.65, Symbol: "Service.Request"},
		{Chunk: Chunk{Path: "tools/helper.go", StartLine: 1, EndLine: 30, Content: "workspace helper reads configuration for tools"}, Score: 0.79, Symbol: "loadWorkspaceConfig"},
	}

	reranked := RerankSearchResults(t.TempDir(), "central permission approval path governing workspace reads and writes across all tools", results, 4)

	require.Len(t, reranked, 4)
	require.Equal(t, "permission/service.go", reranked[0].Chunk.Path)
	require.Equal(t, SearchRoleDirect, reranked[0].Role)
	require.NotEqual(t, "tools/helper.go", reranked[1].Chunk.Path)
}

func TestRerankSearchResultsTracesDurableDeliveryAcrossMechanisms(t *testing.T) {
	results := []SearchResult{
		{Chunk: Chunk{Path: "pkg/task/worker.go", StartLine: 40, EndLine: 80, Content: "background task completion creates and persists a durable notification"}, Score: 0.78, Symbol: "Task.finish"},
		{Chunk: Chunk{Path: "pkg/app/notifications.go", StartLine: 10, EndLine: 45, Content: "subscribe to completed task notifications and deliver each notification to the parent agent"}, Score: 0.67, Symbol: "App.startTaskNotificationDelivery"},
		{Chunk: Chunk{Path: "pkg/agent/notifications.go", StartLine: 20, EndLine: 52, Content: "deliver structured task notification into parent agent session and invoke persisted or discarded callbacks"}, Score: 0.66, Symbol: "Coordinator.DeliverTaskNotification"},
		{Chunk: Chunk{Path: "pkg/task/metadata.go", StartLine: 100, EndLine: 145, Content: "list undelivered durable notifications and persist model delivered timestamp"}, Score: 0.61, Symbol: "Store.MarkNotificationDelivered"},
		{Chunk: Chunk{Path: "pkg/app/notifications.go", StartLine: 47, EndLine: 64, Content: "retry undelivered notification after failed parent agent injection"}, Score: 0.59, Symbol: "App.retryTaskNotification"},
		{Chunk: Chunk{Path: "pkg/task/notification.go", StartLine: 1, EndLine: 25, Content: "construct durable task notification payload for delivery injection into parent agent after completion"}, Score: 0.58, Symbol: "newTaskNotification"},
		{Chunk: Chunk{Path: "pkg/app/notifications_test.go", StartLine: 10, EndLine: 40, Content: "concurrent task notification delivery admits exactly one parent injection and retries after discard"}, Score: 0.60, Symbol: "TestNotificationDeliveryDeduplicatesAndRetries"},
		{Chunk: Chunk{Path: "docs/task-notifications.md", StartLine: 30, EndLine: 45, Content: "durable notification delivery contract requires exactly once parent agent injection across restart"}, Score: 0.51},
		{Chunk: Chunk{Path: "pkg/agent/continuation.go", StartLine: 1, EndLine: 70, Content: "continue background agent transcript using parent session after restart"}, Score: 0.74, Symbol: "ContinueAgent"},
		{Chunk: Chunk{Path: "pkg/ui/completion.go", StartLine: 1, EndLine: 45, Content: "show ordinary foreground agent completed notification in user interface"}, Score: 0.72, Symbol: "UI.AgentFinished"},
	}

	reranked := RerankSearchResults(t.TempDir(), "background task completion durable notification delivered to parent agent concurrent delivery deduplication persisted retry after restart injection", results, 8)

	require.Len(t, reranked, 8)
	require.Equal(t, "pkg/task/worker.go", reranked[0].Chunk.Path)
	bySymbol := make(map[string]SearchRole)
	paths := make(map[string]bool)
	for _, result := range reranked {
		bySymbol[result.Symbol] = result.Role
		paths[result.Chunk.Path] = true
	}
	require.Equal(t, SearchRoleDelivery, bySymbol["App.startTaskNotificationDelivery"])
	require.Equal(t, SearchRoleDelivery, bySymbol["Coordinator.DeliverTaskNotification"])
	require.Equal(t, SearchRolePersistence, bySymbol["Store.MarkNotificationDelivered"])
	require.Equal(t, SearchRoleRecovery, bySymbol["App.retryTaskNotification"])
	require.Equal(t, SearchRoleConstruction, bySymbol["newTaskNotification"])
	require.Equal(t, SearchRoleValidation, bySymbol["TestNotificationDeliveryDeduplicatesAndRetries"])
	require.True(t, paths["docs/task-notifications.md"])
	require.False(t, paths["pkg/agent/continuation.go"])
	require.False(t, paths["pkg/ui/completion.go"])
}

func TestRerankSearchResultsPenalizesSyntheticSourceFixtures(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "search", "rerank_test.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	source := `package search

import "testing"

func TestSyntheticSearchFixture(t *testing.T) {
	source := "background shell recovery after restart converts running task to lost"
	_ = source
}
`
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	results := []SearchResult{
		{Chunk: Chunk{Path: "runtime/recovery.go", StartLine: 1, EndLine: 20, Content: "background shell recovery after restart converts running task to lost"}, Score: 0.72, Symbol: "Manager.recover"},
		{Chunk: Chunk{Path: "runtime/recovery_test.go", StartLine: 1, EndLine: 20, Content: "test running task becomes lost after restart recovery"}, Score: 0.65, Symbol: "TestManagerRecovery"},
		{Chunk: Chunk{Path: "search/rerank_test.go", StartLine: 5, EndLine: 8, Content: "background shell recovery after restart converts running task to lost"}, Score: 0.88},
	}

	reranked := RerankSearchResults(root, "background shell recovery after restart converts running task to lost", results, 2)

	require.Len(t, reranked, 2)
	require.Equal(t, "runtime/recovery.go", reranked[0].Chunk.Path)
	require.Equal(t, "runtime/recovery_test.go", reranked[1].Chunk.Path)
}

func TestRerankSearchResultsPrefersOrderedPolicyPathOverRepeatedCallers(t *testing.T) {
	results := []SearchResult{
		{Chunk: Chunk{Path: "permission/service.go", StartLine: 40, EndLine: 110, Content: "detached agent permission request applies established approvals then denies unresolved access before publishing or waiting for an interactive response"}, Score: 0.82, Symbol: "Service.Request"},
		{Chunk: Chunk{Path: "agent/hooked_tool.go", StartLine: 20, EndLine: 65, Content: "run PreToolUse hooks first; deny or halt, rewrite input, stamp call scoped approval, then invoke inner.Run"}, Score: 0.61, Symbol: "hookedTool.Run"},
		{Chunk: Chunk{Path: "permission/service_test.go", StartLine: 70, EndLine: 130, Content: "Service.Request denies an unresolved detached request and asserts no interactive request was published or awaited while established approval grants remain valid"}, Score: 0.60, Symbol: "TestDetachedRequestDoesNotWait"},
		{Chunk: Chunk{Path: "agent/hooked_tool_test.go", StartLine: 20, EndLine: 55, Content: "hook deny and halt stop inner execution while hook allow stamps approval before invoking the tool"}, Score: 0.57, Symbol: "TestHookOrdering"},
		{Chunk: Chunk{Path: "tools/view.go", StartLine: 1, EndLine: 45, Content: "view file after permissions.Request(ctx, operation)"}, Score: 0.79, Symbol: "View.Run"},
		{Chunk: Chunk{Path: "tools/download.go", StartLine: 1, EndLine: 45, Content: "download file after permissions.Request(ctx, operation)"}, Score: 0.78, Symbol: "Download.Run"},
		{Chunk: Chunk{Path: "tools/bash.go", StartLine: 1, EndLine: 45, Content: "execute command after permissions.Request(ctx, operation)"}, Score: 0.77, Symbol: "Bash.Run"},
		{Chunk: Chunk{Path: "ui/agent_form.go", StartLine: 1, EndLine: 40, Content: "build a background agent definition request from form fields"}, Score: 0.75, Symbol: "Form.BuildRequest"},
	}

	reranked := RerankSearchResults(t.TempDir(), "background agent tool permission runs hooks first then denies unresolved access without waiting for interactive user response", results, 6)

	require.Len(t, reranked, 6)
	bySymbol := make(map[string]SearchRole)
	callerCount := 0
	for _, result := range reranked {
		bySymbol[result.Symbol] = result.Role
		if result.Symbol == "View.Run" || result.Symbol == "Download.Run" || result.Symbol == "Bash.Run" {
			callerCount++
		}
	}
	require.Equal(t, SearchRoleDirect, bySymbol["Service.Request"])
	require.Contains(t, bySymbol, "hookedTool.Run")
	require.Equal(t, SearchRoleValidation, bySymbol["TestDetachedRequestDoesNotWait"])
	require.Equal(t, SearchRoleValidation, bySymbol["TestHookOrdering"])
	require.LessOrEqual(t, callerCount, 2)
	require.NotContains(t, bySymbol, "Form.BuildRequest")
}

func TestRerankSearchResultsExpandsConnectedDeliveryHelpers(t *testing.T) {
	results := []SearchResult{
		{Chunk: Chunk{Path: "app/notifications.go", StartLine: 70, EndLine: 110, Content: "deliver notification: beginDelivery(id); coordinator.Deliver(ctx, notification); finishDelivery(id); retryDelivery(notification)"}, Score: 0.80, Symbol: "App.deliverNotification"},
		{Chunk: Chunk{Path: "agent/notifications.go", StartLine: 10, EndLine: 40, Content: "encode structured notification and inject it into the parent session with persisted and discarded callbacks"}, Score: 0.76, Symbol: "Coordinator.Deliver"},
		{Chunk: Chunk{Path: "app/notifications.go", StartLine: 1, EndLine: 35, Content: "startup loads pending durable notifications and subscribes to live events before calling deliverNotification(ctx, notification)"}, Score: 0.68, Symbol: "App.startNotificationDelivery"},
		{Chunk: Chunk{Path: "app/notifications.go", StartLine: 112, EndLine: 125, Content: "atomically reserve one in flight delivery by notification identity"}, Score: 0.51, Symbol: "App.beginDelivery"},
		{Chunk: Chunk{Path: "app/notifications.go", StartLine: 127, EndLine: 138, Content: "release the in flight notification delivery reservation"}, Score: 0.50, Symbol: "App.finishDelivery"},
		{Chunk: Chunk{Path: "app/notifications.go", StartLine: 140, EndLine: 155, Content: "retry undelivered notification after discard or persistence failure"}, Score: 0.52, Symbol: "App.retryDelivery"},
		{Chunk: Chunk{Path: "task/store.go", StartLine: 50, EndLine: 90, Content: "persist delivered timestamp and list pending undelivered notification records after restart"}, Score: 0.58, Symbol: "Store.MarkDelivered"},
		{Chunk: Chunk{Path: "app/notifications_test.go", StartLine: 20, EndLine: 75, Content: "App.deliverNotification concurrently admits one injection, retries discarded delivery, and does not reinject a delivered record after restart"}, Score: 0.55, Symbol: "TestDeliveryExactlyOnceAcrossRestart"},
		{Chunk: Chunk{Path: "mcp/channel.go", StartLine: 1, EndLine: 70, Content: "buffer MCP channel notification during protocol negotiation and reconnect"}, Score: 0.74, Symbol: "Channel.Notify"},
		{Chunk: Chunk{Path: "agent/run.go", StartLine: 1, EndLine: 60, Content: "coalesce foreground agent RunComplete events and retry queue processing"}, Score: 0.72, Symbol: "Agent.run"},
	}

	reranked := RerankSearchResults(t.TempDir(), "deliver completed task notification to parent exactly once with concurrent deduplication durable acknowledgement retry and restart recovery", results, 8)

	require.Len(t, reranked, 8)
	bySymbol := make(map[string]SearchRole)
	for _, result := range reranked {
		bySymbol[result.Symbol] = result.Role
	}
	require.Equal(t, SearchRoleDirect, bySymbol["App.deliverNotification"])
	require.Contains(t, bySymbol, "App.startNotificationDelivery")
	require.Contains(t, bySymbol, "App.beginDelivery")
	require.Contains(t, bySymbol, "App.retryDelivery")
	require.Equal(t, SearchRoleValidation, bySymbol["TestDeliveryExactlyOnceAcrossRestart"])
	require.Equal(t, SearchRolePersistence, bySymbol["Store.MarkDelivered"])
	require.NotContains(t, bySymbol, "Channel.Notify")
	require.NotContains(t, bySymbol, "Agent.run")
}

func TestInferSearchRoleDoesNotTreatGenericConstructorAsPayload(t *testing.T) {
	direct := SearchResult{Symbol: "Manager.recover"}
	constructor := SearchResult{Symbol: "NewShell", Chunk: Chunk{Path: "shell/shell.go", Content: "return &Shell{environment: environment}"}}

	require.Equal(t, SearchRoleRelated, inferSearchRole(constructor, direct, significantSearchTerms("background shell restart recovery marks running work lost")))
}

func TestSearchTermMatchesRelatedWordForms(t *testing.T) {
	require.True(t, searchTermMatches("delivery", map[string]struct{}{"delivered": {}}))
	require.True(t, searchTermMatches("persistence", map[string]struct{}{"persisted": {}}))
	require.True(t, searchTermMatches("deduplication", map[string]struct{}{"deduplicated": {}}))
	require.False(t, searchTermMatches("delivery", map[string]struct{}{"delegate": {}}))
}

func TestRerankSearchResultsDeduplicatesOverlappingNonGoChunks(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	lines := make([]string, 40)
	for index := range lines {
		lines[index] = "line content"
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "task.ts"), []byte(strings.Join(lines, "\n")), 0o600))
	results := []SearchResult{
		{Chunk: Chunk{Path: "src/task.ts", ChunkIndex: 1, StartLine: 10, EndLine: 30, Content: "restart running lost"}, Score: 0.90},
		{Chunk: Chunk{Path: "src/task.ts", ChunkIndex: 2, StartLine: 20, EndLine: 35, Content: "running lost notification"}, Score: 0.85},
		{Chunk: Chunk{Path: "src/store.ts", ChunkIndex: 0, StartLine: 1, EndLine: 10, Content: "persist task"}, Score: 0.70},
	}

	reranked := RerankSearchResults(root, "restart running lost", results, 10)

	require.Len(t, reranked, 2)
	require.Equal(t, "src/task.ts", reranked[0].Chunk.Path)
	require.Equal(t, 10, reranked[0].Chunk.StartLine)
	require.Equal(t, 35, reranked[0].Chunk.EndLine)
}
