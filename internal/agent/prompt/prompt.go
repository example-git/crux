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
	"strings"
	"text/template"
	"time"

	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/filepathext"
	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/shell"
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
	systemPromptOverrideDir string
	projectService          *projects.Service
}

type LifecycleStage string

const (
	LifecycleDefault   LifecycleStage = "default"
	LifecycleDraft     LifecycleStage = "plan"
	LifecycleRevision  LifecycleStage = "plan_revision"
	LifecycleExecution LifecycleStage = "plan_execution"
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
	GitStatus          string
	NativeSections     string // Pre-built native instruction sections.
	Lifecycle          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
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

func WithInstructions(instructions string) Option {
	return func(p *Prompt) {
		p.instructions = instructions
	}
}

func withSystemPromptOverrideDir(dir string) Option {
	return func(p *Prompt) {
		p.systemPromptOverrideDir = dir
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
		systemPromptOverrideDir: filepath.Join(home.Dir(), ".ai-cli", "instructions"),
		projectService:          projects.NewService(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
	return p.BuildLifecycle(ctx, provider, model, store, Lifecycle{Stage: LifecycleDefault})
}

func (p *Prompt) BuildLifecycle(ctx context.Context, provider, model string, store *config.ConfigStore, lifecycle Lifecycle) (string, error) {
	if err := validateLifecycle(lifecycle); err != nil {
		return "", err
	}
	if lifecycle.Stage != LifecycleDefault && p.name != "coder" {
		return "", fmt.Errorf("prompt %q does not support lifecycle stage %q", p.name, lifecycle.Stage)
	}
	if lifecycle.Stage == LifecycleDefault {
		if override, ok, err := p.systemPromptOverride(provider, store.Config()); err != nil {
			return "", err
		} else if ok {
			return p.appendCoderContext(ctx, override, store)
		}
	}

	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d, err := p.promptData(ctx, provider, model, store, lifecycle)
	if err != nil {
		return "", err
	}
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	if lifecycle.Stage == LifecycleDefault {
		if err := p.initializeSystemPromptOverride(provider, store.Config(), sb.String()); err != nil {
			return "", err
		}
	}

	return p.appendCoderContext(ctx, sb.String(), store)
}

func (p *Prompt) appendCoderContext(ctx context.Context, base string, store *config.ConfigStore) (string, error) {
	withProject, err := p.appendProject(base, store)
	if err != nil {
		return "", err
	}
	return p.appendMemory(ctx, withProject, store)
}

func (p *Prompt) appendProject(base string, store *config.ConfigStore) (string, error) {
	if p.name != "coder" {
		return base, nil
	}
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	if workingDir == "" {
		return base, nil
	}
	document, ok, err := p.projectService.Active(workingDir)
	if err != nil {
		return "", err
	}
	if !ok {
		return base, nil
	}
	var builder strings.Builder
	builder.WriteString(strings.TrimRight(base, "\n"))
	builder.WriteString("\n\n<persistent_project>\n")
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
	builder.WriteString("</persistent_project>\n")
	return builder.String(), nil
}

func (p *Prompt) appendMemory(ctx context.Context, base string, store *config.ConfigStore) (string, error) {
	if p.name != "coder" {
		return base, nil
	}
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	if workingDir == "" {
		return base, nil
	}
	memory, err := automemory.Load(ctx, workingDir)
	if err != nil {
		if _, ok := errors.AsType[*automemory.ConfigurationError](err); ok {
			return "", err
		}
		slog.Debug("Failed to load auto memory", "error", err)
		return base, nil
	}
	memoryPrompt := automemory.Prompt(memory)
	if memoryPrompt == "" {
		return base, nil
	}
	return strings.TrimRight(base, "\n") + "\n\n" + memoryPrompt + "\n", nil
}

func (p *Prompt) systemPromptOverride(provider string, cfg *config.Config) (string, bool, error) {
	if p.name != "coder" || cfg.Options == nil || !cfg.Options.SystemPromptOverride {
		return "", false, nil
	}
	if err := validateSystemPromptOverrideProvider(provider); err != nil {
		return "", false, err
	}

	path := filepath.Join(p.systemPromptOverrideDir, provider+".txt")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading system prompt override %q: %w", path, err)
	}
	return string(content), true, nil
}

func (p *Prompt) initializeSystemPromptOverride(provider string, cfg *config.Config, content string) error {
	if p.name != "coder" || cfg.Options == nil || !cfg.Options.SystemPromptOverride {
		return nil
	}
	if err := validateSystemPromptOverrideProvider(provider); err != nil {
		return err
	}
	if err := os.MkdirAll(p.systemPromptOverrideDir, 0o755); err != nil {
		return fmt.Errorf("creating system prompt override directory %q: %w", p.systemPromptOverrideDir, err)
	}
	path := filepath.Join(p.systemPromptOverrideDir, provider+".txt")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating system prompt override %q: %w", path, err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("initializing system prompt override %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing system prompt override %q: %w", path, err)
	}
	return nil
}

func validateSystemPromptOverrideProvider(provider string) error {
	if provider == "" || provider == "." || provider == ".." || strings.ContainsAny(provider, `/\\`) {
		return fmt.Errorf("invalid provider ID %q for system prompt override", provider)
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
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of paths.
func loadContextFiles(paths []string, store *config.ConfigStore) map[string][]ContextFile {
	files := map[string][]ContextFile{}
	for _, pth := range paths {
		expanded := expandPath(pth, store)
		pathKey := strings.ToLower(expanded)
		if _, ok := files[pathKey]; ok {
			continue
		}
		files[pathKey] = processContextPath(expanded, store)
	}
	return files
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore, lifecycle Lifecycle) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	cfg := store.Config()
	contextFiles := loadContextFiles(cfg.Options.ContextPaths, store)
	globalContextFiles := loadContextFiles(cfg.Options.GlobalContextPaths, store)

	// Discover and load skills metadata.
	var availSkillXML string

	// Start with builtin skills.
	allSkills := skills.DiscoverBuiltin()
	builtinNames := make(map[string]bool, len(allSkills))
	for _, s := range allSkills {
		builtinNames[s.Name] = true
	}

	// Discover user skills from configured paths.
	if len(cfg.Options.SkillsPaths) > 0 {
		expandedPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
		for _, pth := range cfg.Options.SkillsPaths {
			expandedPaths = append(expandedPaths, expandPath(pth, store))
		}
		for _, userSkill := range skills.Discover(expandedPaths) {
			if builtinNames[userSkill.Name] {
				slog.Warn("User skill overrides builtin skill", "name", userSkill.Name)
			}
			allSkills = append(allSkills, userSkill)
		}
	}

	// Deduplicate: user skills override builtins with the same name.
	allSkills = skills.Deduplicate(allSkills)

	// Filter out disabled skills.
	allSkills = skills.Filter(allSkills, cfg.Options.DisabledSkills)

	if len(allSkills) > 0 {
		availSkillXML = skills.ToPromptXML(allSkills)
	}

	var nativeSections string
	mode := cmp.Or(cfg.Options.InstructionMode, "all")
	if mode != "project" {
		instructions, err := lifecycleToolingInstructions(provider, cfg, lifecycle.Stage)
		if err != nil {
			return PromptDat{}, err
		}
		nativeSections = instructions
	}

	isGit := isGitRepo(store.WorkingDir())
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
	}
	if isGit {
		var err error
		data.GitStatus, err = getGitStatus(ctx, store.WorkingDir())
		if err != nil {
			return PromptDat{}, err
		}
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
	profile := config.ToolingInstructionsCrux
	if cfg.Providers != nil {
		if providerCfg, ok := cfg.Providers.Get(providerID); ok {
			profile = cmp.Or(providerCfg.ToolingInstructions, config.ToolingInstructionsCrux)
		}
	}

	switch profile {
	case config.ToolingInstructionsCrux:
		sections := FilterSections(AllSections(), cfg.Options.DisabledInstructionSections)
		return SectionsToString(sections), nil
	case config.ToolingInstructionsNative:
		registration, ok := config.ProviderBehaviorCapabilities(providerID)
		if !ok || registration.ProviderID != providerID || registration.Instructions == nil {
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

func getGitStatus(ctx context.Context, dir string) (string, error) {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch, err := getGitBranch(ctx, sh)
	if err != nil {
		return "", err
	}
	status, err := getGitStatusSummary(ctx, sh)
	if err != nil {
		return "", err
	}
	commits, err := getGitRecentCommits(ctx, sh)
	if err != nil {
		return "", err
	}
	return branch + status + commits, nil
}

func getGitBranch(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git branch --show-current 2>/dev/null")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	return fmt.Sprintf("Current branch: %s\n", out), nil
}

func getGitStatusSummary(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git status --short 2>/dev/null | head -20")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n", nil
	}
	return fmt.Sprintf("Status:\n%s\n", out), nil
}

func getGitRecentCommits(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git log --oneline -n 3 2>/dev/null")
	if err != nil || out == "" {
		return "", nil
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out), nil
}

func (p *Prompt) Name() string {
	return p.name
}
