package prompt

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/template"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/filepathext"
	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/skills"
)

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name                    string
	template                string
	instructions            string
	now                     func() time.Time
	platform                string
	workingDir              string
	providerInstructionsDir string
	projectService          *projects.Service
	activeSkills            []*skills.Skill
	activeSkillsSet         bool
}

type LifecycleStage string

const (
	LifecycleDefault   LifecycleStage = "default"
	LifecycleDraft     LifecycleStage = "plan"
	LifecycleRevision  LifecycleStage = "plan_revision"
	LifecycleExecution LifecycleStage = "plan_execution"

	skillUsageInstructions = "<skills_usage>\n" +
		"The `<description>` of each skill is a TRIGGER — it tells you *when* a skill applies. It is NOT a specification of what the skill does or how to do it. The procedure, scripts, commands, references, and required flags live only in the SKILL.md body.\n\n" +
		"MANDATORY activation flow:\n" +
		"1. Scan `<available_skills>` against the current user task.\n" +
		"2. If any skill's `<description>` matches, call the skill_load tool with its name before any other tool call that performs the task.\n" +
		"3. Read the entire SKILL.md and follow its instructions.\n" +
		"4. Only then execute the task, using the skill's prescribed commands/tools.\n\n" +
		"Do NOT skip step 2. Do NOT infer a skill's behavior from its name or description. If you find yourself about to run a task-doing tool for a skill-eligible request without having just loaded the SKILL.md, stop and load the skill first.\n" +
		"</skills_usage>"
)

type Lifecycle struct {
	Stage LifecycleStage
	Plan  string
}

type PromptDat struct {
	Provider           string
	Model              string
	Instructions       string
	Config             config.Config
	WorkingDir         string
	IsGitRepo          bool
	Platform           string
	Date               string
	RenderDate         bool
	NativeSections     string // Pre-built native instruction sections.
	Lifecycle          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
	SkillUsage         string
}

type ContextFile struct {
	Path    string
	Content string
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

// WithSkills binds the prompt to the coordinator's effective active skill
// snapshot so model-facing instructions match skill_list and skill_load.
func WithSkills(active []*skills.Skill) Option {
	return func(p *Prompt) {
		p.activeSkills = slices.Clone(active)
		p.activeSkillsSet = true
	}
}

func WithInstructions(instructions string) Option {
	return func(p *Prompt) {
		p.instructions = instructions
	}
}

func withProviderInstructionsDir(dir string) Option {
	return func(p *Prompt) {
		p.providerInstructionsDir = dir
	}
}

func withProjectService(service *projects.Service) Option {
	return func(p *Prompt) {
		p.projectService = service
	}
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:                    name,
		template:                promptTemplate,
		now:                     time.Now,
		providerInstructionsDir: filepath.Join(home.Dir(), ".ai-cli", "instructions"),
		projectService:          projects.NewService(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
	instructions, err := p.BuildInstructions(ctx, provider, model, store)
	if err != nil {
		return "", err
	}
	return instructions.String(), nil
}

func (p *Prompt) BuildInstructions(ctx context.Context, provider, model string, store *config.ConfigStore) (fantasy.Instructions, error) {
	return p.BuildLifecycleInstructions(ctx, provider, model, store, Lifecycle{Stage: LifecycleDefault})
}

func (p *Prompt) BuildInstructionsWithSnapshot(ctx context.Context, provider, model string, store *config.ConfigStore, snapshot config.RuntimeSnapshot) (fantasy.Instructions, error) {
	return p.buildLifecycleInstructions(ctx, provider, model, store, snapshot.Config(), snapshot.Resolve, Lifecycle{Stage: LifecycleDefault})
}

func (p *Prompt) BuildLifecycle(ctx context.Context, provider, model string, store *config.ConfigStore, lifecycle Lifecycle) (string, error) {
	instructions, err := p.BuildLifecycleInstructions(ctx, provider, model, store, lifecycle)
	if err != nil {
		return "", err
	}
	return instructions.String(), nil
}

func (p *Prompt) BuildLifecycleInstructions(ctx context.Context, provider, model string, store *config.ConfigStore, lifecycle Lifecycle) (fantasy.Instructions, error) {
	snapshot := store.RuntimeSnapshot()
	return p.buildLifecycleInstructions(ctx, provider, model, store, snapshot.Config(), snapshot.Resolve, lifecycle)
}

func (p *Prompt) BuildLifecycleInstructionsWithSnapshot(ctx context.Context, provider, model string, store *config.ConfigStore, snapshot config.RuntimeSnapshot, lifecycle Lifecycle) (fantasy.Instructions, error) {
	return p.buildLifecycleInstructions(ctx, provider, model, store, snapshot.Config(), snapshot.Resolve, lifecycle)
}

func (p *Prompt) buildLifecycleInstructions(ctx context.Context, provider, model string, store *config.ConfigStore, cfg *config.Config, resolve func(string) (string, error), lifecycle Lifecycle) (fantasy.Instructions, error) {
	if err := validateLifecycle(lifecycle); err != nil {
		return fantasy.Instructions{}, err
	}
	if lifecycle.Stage != LifecycleDefault && p.name != "coder" {
		return fantasy.Instructions{}, fmt.Errorf("prompt %q does not support lifecycle stage %q", p.name, lifecycle.Stage)
	}
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return fantasy.Instructions{}, fmt.Errorf("parsing template: %w", err)
	}
	data, err := p.promptData(ctx, provider, model, store, cfg, resolve, lifecycle)
	if err != nil {
		return fantasy.Instructions{}, err
	}
	structuredDate := strings.Contains(p.template, ".RenderDate")
	runtimeInstructions := ""
	if structuredDate {
		runtimeInstructions = "Today's date: " + data.Date
	}
	var nativeInstructions string
	var lifecycleInstructions string
	if p.name == "coder" {
		if strings.Contains(p.template, ".NativeSections") {
			nativeInstructions = data.NativeSections
			data.NativeSections = ""
		}
		if strings.Contains(p.template, ".Lifecycle") {
			lifecycleInstructions = data.Lifecycle
			data.Lifecycle = ""
		}
	}
	var builder strings.Builder
	if err := t.Execute(&builder, data); err != nil {
		return fantasy.Instructions{}, fmt.Errorf("executing template: %w", err)
	}

	renderedStability := fantasy.InstructionStabilityStatic
	if strings.Contains(p.template, ".Date") && !structuredDate {
		renderedStability = fantasy.InstructionStabilityDynamic
	}
	renderedKind := fantasy.InstructionKindAuxiliary
	if p.name == "coder" {
		renderedKind = fantasy.InstructionKindEnvironment
	}
	renderedInstructions := fantasy.InstructionSection{
		Kind:      renderedKind,
		Stability: renderedStability,
		Text:      builder.String(),
	}
	providerInstructions, err := p.providerInstructions(provider)
	if err != nil {
		return fantasy.Instructions{}, err
	}
	var instructions fantasy.Instructions
	if p.name == "coder" {
		instructions = fantasy.NewInstructions(
			fantasy.StaticInstruction(fantasy.InstructionKindTooling, nativeInstructions),
			fantasy.DynamicInstruction(fantasy.InstructionKindProviderContext, providerInstructions),
			renderedInstructions,
			fantasy.DynamicInstruction(fantasy.InstructionKindLifecycle, lifecycleInstructions),
			fantasy.DynamicInstruction(fantasy.InstructionKindRuntime, runtimeInstructions),
		)
	} else {
		instructions = fantasy.NewInstructions(
			renderedInstructions,
			fantasy.DynamicInstruction(fantasy.InstructionKindRuntime, runtimeInstructions),
		)
	}
	instructions, err = p.appendCoderContextInstructions(ctx, instructions, store)
	if err != nil {
		return fantasy.Instructions{}, err
	}
	return instructions, nil
}

func (p *Prompt) appendCoderContextInstructions(ctx context.Context, base fantasy.Instructions, store *config.ConfigStore) (fantasy.Instructions, error) {
	projectInstructions, err := p.projectInstructions(store)
	if err != nil {
		return fantasy.Instructions{}, err
	}
	memoryInstructions, err := p.memoryInstructions(ctx, store)
	if err != nil {
		return fantasy.Instructions{}, err
	}
	return base.Append(
		fantasy.DynamicInstruction(fantasy.InstructionKindProjectState, projectInstructions),
		fantasy.DynamicInstruction(fantasy.InstructionKindMemory, memoryInstructions),
	), nil
}

func (p *Prompt) projectInstructions(store *config.ConfigStore) (string, error) {
	if p.name != "coder" {
		return "", nil
	}
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	if workingDir == "" {
		return "", nil
	}
	document, ok, err := p.projectService.Active(workingDir)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	var builder strings.Builder
	builder.WriteString("<persistent_project>\n")
	builder.WriteString("# Active Project\n\n")
	builder.WriteString("The following durable project is selected for this workspace. Treat the main project file as the authoritative goal and task state, and the notes file as durable supporting context. Keep Projects separate from plans. Use the existing `todos` tool to track the incomplete subtasks of the current project goal during this session. Use the typed project tools to make durable project changes.\n\n")
	if goal, subtasks, found := document.CurrentGoal(); found {
		fmt.Fprintf(&builder, "Current goal: `%s` %s\n", goal.ID, goal.Content)
		if len(subtasks) > 0 {
			builder.WriteString("Current incomplete subtasks:\n")
			for _, subtask := range subtasks {
				fmt.Fprintf(&builder, "- `%s` %s\n", subtask.ID, subtask.Content)
			}
		}
		builder.WriteString("\n")
	}
	fmt.Fprintf(&builder, "<file path=%q>\n%s\n</file>\n", filepath.ToSlash(document.Path), document.Content)
	fmt.Fprintf(&builder, "<file path=%q>\n%s\n</file>\n", filepath.ToSlash(document.NotesPath), document.Notes)
	builder.WriteString("</persistent_project>")
	return builder.String(), nil
}

func (p *Prompt) memoryInstructions(ctx context.Context, store *config.ConfigStore) (string, error) {
	if p.name != "coder" {
		return "", nil
	}
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	if workingDir == "" {
		return "", nil
	}
	memory, err := automemory.Load(ctx, workingDir)
	if err != nil {
		if _, ok := errors.AsType[*automemory.ConfigurationError](err); ok {
			return "", err
		}
		slog.Debug("Failed to load auto memory", "error", err)
		return "", nil
	}
	return automemory.Prompt(memory), nil
}

func (p *Prompt) providerInstructions(provider string) (string, error) {
	if p.name != "coder" || provider == "" {
		return "", nil
	}
	if err := validateProviderInstructionsID(provider); err != nil {
		return "", err
	}
	path := filepath.Join(p.providerInstructionsDir, provider+".txt")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading provider instructions %q: %w", path, err)
	}
	return string(content), nil
}

func validateProviderInstructionsID(provider string) error {
	if provider == "" || provider == "." || provider == ".." || strings.ContainsAny(provider, `/\\`) {
		return fmt.Errorf("invalid provider ID %q for provider instructions", provider)
	}
	return nil
}

func processFile(filePath string) *ContextFile {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	return &ContextFile{
		Path:    filePath,
		Content: string(content),
	}
}

func processContextPath(p string, store *config.ConfigStore) []ContextFile {
	var contexts []ContextFile
	fullPath := filepathext.SmartJoin(store.WorkingDir(), p)
	info, err := os.Stat(fullPath)
	if err != nil {
		return contexts
	}
	if info.IsDir() {
		filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if result := processFile(path); result != nil {
					contexts = append(contexts, *result)
				}
			}
			return nil
		})
	} else {
		result := processFile(fullPath)
		if result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, resolve func(string) (string, error)) string {
	path = home.Long(path)
	if strings.HasPrefix(path, "$") {
		if expanded, err := resolve(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of paths.
func loadContextFiles(paths []string, store *config.ConfigStore, resolve func(string) (string, error)) map[string][]ContextFile {
	files := map[string][]ContextFile{}
	for _, pth := range paths {
		expanded := expandPath(pth, resolve)
		pathKey := strings.ToLower(expanded)
		if _, ok := files[pathKey]; ok {
			continue
		}
		files[pathKey] = processContextPath(expanded, store)
	}
	return files
}

func (p *Prompt) availableSkillXML(store *config.ConfigStore, cfg *config.Config, resolve func(string) (string, error)) string {
	if p.activeSkillsSet {
		return skills.ToPromptXML(p.activeSkills)
	}

	_, active, _ := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths:    cfg.Options.SkillsPaths,
		DisabledSkills: cfg.Options.DisabledSkills,
		WorkingDir:     store.WorkingDir(),
		Resolver:       resolve,
	})
	return skills.ToPromptXML(active)
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore, cfg *config.Config, resolve func(string) (string, error), lifecycle Lifecycle) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	contextFiles := loadContextFiles(cfg.Options.ContextPaths, store, resolve)
	globalContextFiles := loadContextFiles(cfg.Options.GlobalContextPaths, store, resolve)

	availSkillXML := p.availableSkillXML(store, cfg, resolve)

	var nativeSections string
	mode := cmp.Or(cfg.Options.InstructionMode, "all")
	if mode != "project" {
		instructions, err := lifecycleToolingInstructions(provider, cfg, lifecycle.Stage)
		if err != nil {
			return PromptDat{}, err
		}
		nativeSections = instructions
	}

	isGit := isGitRepo(workingDir)
	data := PromptDat{
		Provider:       provider,
		Model:          model,
		Instructions:   p.instructions,
		Config:         *cfg,
		WorkingDir:     filepath.ToSlash(workingDir),
		IsGitRepo:      isGit,
		Platform:       platform,
		Date:           p.now().Format("1/2/2006"),
		NativeSections: nativeSections,
		Lifecycle:      lifecycleInstructions(lifecycle),
		AvailSkillXML:  availSkillXML,
		SkillUsage:     skillUsageInstructions,
	}
	// In "native" mode, skip project/global context files.
	if mode != "native" {
		for _, files := range contextFiles {
			data.ContextFiles = append(data.ContextFiles, files...)
		}
		for _, files := range globalContextFiles {
			data.GlobalContextFiles = append(data.GlobalContextFiles, files...)
		}
	}
	return data, nil
}

func toolingInstructions(providerID string, cfg *config.Config) (string, error) {
	registration, hasRegistration := cfg.ProviderBehaviorRegistration(providerID)
	profile := config.ToolingInstructionsCrux
	if hasRegistration && registration.ProviderID == providerID && registration.Instructions != nil {
		profile = cmp.Or(registration.Instructions.SelectionDefault, config.ToolingInstructionsCrux)
	}
	if cfg.Providers != nil {
		if providerCfg, ok := cfg.Providers.Get(providerID); ok && providerCfg.ToolingInstructions != "" {
			profile = providerCfg.ToolingInstructions
		}
	}

	switch profile {
	case config.ToolingInstructionsCrux:
		sections := FilterSections(AllSections(), cfg.Options.DisabledInstructionSections)
		return SectionsToString(sections), nil
	case config.ToolingInstructionsNative:
		if !hasRegistration || registration.ProviderID != providerID || registration.Instructions == nil {
			return "", fmt.Errorf("provider %q does not provide native tooling instructions", providerID)
		}
		text, ok := registration.Instructions.Profiles[registration.Instructions.Default]
		if !ok {
			return "", fmt.Errorf("provider %q has no default native tooling instruction profile", providerID)
		}
		return text, nil
	default:
		return "", fmt.Errorf("unsupported tooling instruction profile %q for provider %q", profile, providerID)
	}
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func (p *Prompt) Name() string {
	return p.name
}
