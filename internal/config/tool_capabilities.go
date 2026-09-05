package config

import "slices"

const (
	capabilityApplicationRead  = "application_read"
	capabilityApplicationWrite = "application_write"
	capabilityExternalRead     = "external_read"
	capabilityInteractive      = "interactive"
	capabilityOrchestration    = "orchestration"
	capabilityProcessControl   = "process_control"
	capabilityProcessExecute   = "process_execute"
	capabilityProcessRead      = "process_read"
	capabilityWorkspaceRead    = "workspace_read"
	capabilityWorkspaceWrite   = "workspace_write"
)

type builtinToolPolicy struct {
	capabilities []string
	planSafe     bool
}

var builtinToolPolicies = map[string]builtinToolPolicy{
	"agent":              {capabilities: []string{capabilityOrchestration}},
	"agentic_fetch":      {capabilities: []string{capabilityExternalRead}, planSafe: true},
	"bash":               {capabilities: []string{capabilityProcessExecute, capabilityWorkspaceWrite}},
	"jq":                 {capabilities: []string{capabilityWorkspaceRead}},
	"codebase_search":    {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"complete_plan":      {capabilities: []string{capabilityApplicationWrite, capabilityInteractive}},
	"crux_info":          {capabilities: []string{capabilityApplicationRead}, planSafe: true},
	"crux_logs":          {capabilities: []string{capabilityApplicationRead}, planSafe: true},
	"traffic_logs":       {capabilities: []string{capabilityApplicationRead}, planSafe: true},
	"traffic_capture":    {capabilities: []string{capabilityExternalRead, capabilityProcessExecute, capabilityWorkspaceWrite}},
	"download":           {capabilities: []string{capabilityExternalRead, capabilityWorkspaceWrite}},
	"edit":               {capabilities: []string{capabilityWorkspaceWrite}},
	"enter_plan":         {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"exit_plan":          {capabilities: []string{capabilityApplicationWrite, capabilityInteractive}, planSafe: true},
	"fetch":              {capabilities: []string{capabilityExternalRead}, planSafe: true},
	"git_inspect":        {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"imagegen":           {capabilities: []string{capabilityExternalRead, capabilityWorkspaceRead, capabilityWorkspaceWrite}},
	"search":             {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"job_kill":           {capabilities: []string{capabilityProcessControl}},
	"job_list":           {capabilities: []string{capabilityProcessRead}, planSafe: true},
	"job_output":         {capabilities: []string{capabilityProcessRead}, planSafe: true},
	"list_mcp_resources": {capabilities: []string{capabilityExternalRead}, planSafe: true},
	"ls":                 {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"lsp_call_hierarchy": {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"lsp_definition":     {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"lsp_diagnostics":    {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"lsp_references":     {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"lsp_rename":         {capabilities: []string{capabilityWorkspaceWrite}},
	"lsp_replace_symbol": {capabilities: []string{capabilityWorkspaceWrite}},
	"lsp_restart":        {capabilities: []string{capabilityProcessControl}},
	"lsp_symbols":        {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"memory_list":        {capabilities: []string{capabilityApplicationRead}, planSafe: true},
	"memory_remove":      {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"memory_upsert":      {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"multiedit":          {capabilities: []string{capabilityWorkspaceWrite}},
	"project_complete":   {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"project_create":     {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"project_notes":      {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"project_status":     {capabilities: []string{capabilityApplicationRead}, planSafe: true},
	"project_update":     {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"question":           {capabilities: []string{capabilityInteractive}, planSafe: true},
	"read_mcp_resource":  {capabilities: []string{capabilityExternalRead}, planSafe: true},
	"script":             {capabilities: []string{capabilityProcessExecute, capabilityWorkspaceWrite}},
	"skill_list":         {capabilities: []string{capabilityApplicationRead}, planSafe: true},
	"skill_load":         {capabilities: []string{capabilityApplicationRead}, planSafe: true},
	"sourcegraph":        {capabilities: []string{capabilityExternalRead}, planSafe: true},
	"task_continue":      {capabilities: []string{capabilityOrchestration}},
	"task_list":          {capabilities: []string{capabilityProcessRead}, planSafe: true},
	"task_output":        {capabilities: []string{capabilityProcessRead}, planSafe: true},
	"task_stop":          {capabilities: []string{capabilityProcessControl}},
	"todos":              {capabilities: []string{capabilityApplicationWrite}, planSafe: true},
	"view":               {capabilities: []string{capabilityWorkspaceRead}, planSafe: true},
	"write":              {capabilities: []string{capabilityWorkspaceWrite}},
}

func ToolCapabilities(name string) ([]string, bool) {
	policy, ok := builtinToolPolicies[name]
	if !ok {
		return nil, false
	}
	return slices.Clone(policy.capabilities), true
}

func IsToolAllowedInPlanMode(name string) bool {
	policy, ok := builtinToolPolicies[name]
	return ok && policy.planSafe
}

func FilterPlanModeTools(names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		if IsToolAllowedInPlanMode(name) {
			result = append(result, name)
		}
	}
	return result
}
