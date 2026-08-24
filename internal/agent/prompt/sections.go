package prompt

import (
	"embed"
	"io/fs"
	"slices"
	"strings"
)

//go:embed sections/*.md
var sectionsFS embed.FS

// SectionID is the identifier for a native instruction section (the file
// name without the .md extension, e.g. "critical_rules", "workflow").
type SectionID = string

// Section is one native instruction section loaded from the embedded FS.
type Section struct {
	ID      SectionID
	Content string
}

// sectionOrder defines the canonical display/injection order. Sections not
// listed here are appended alphabetically after the listed ones.
var sectionOrder = []SectionID{
	"identity",
	"critical_rules",
	"code_references",
	"workflow",
	"operating_constraints",
	"decision_making",
	"editing_files",
	"whitespace",
	"task_completion",
	"memory",
	"code_conventions",
	"tool_usage",
	"proactiveness",
	"final_answers",
}

var excludedSections = map[SectionID]bool{
	"communication_style": true,
	"error_handling":      true,
	"testing":             true,
}

// AllSections returns all embedded sections in canonical order.
func AllSections() []Section {
	entries, err := fs.ReadDir(sectionsFS, "sections")
	if err != nil {
		return nil
	}

	byID := make(map[SectionID]Section, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".md")
		if excludedSections[id] {
			continue
		}
		data, err := sectionsFS.ReadFile("sections/" + entry.Name())
		if err != nil {
			continue
		}
		byID[id] = Section{ID: id, Content: string(data)}
	}

	var result []Section
	seen := make(map[SectionID]bool, len(sectionOrder))
	for _, id := range sectionOrder {
		if s, ok := byID[id]; ok {
			result = append(result, s)
			seen[id] = true
		}
	}
	// Append any sections not in the canonical order (alphabetically).
	var extras []SectionID
	for id := range byID {
		if !seen[id] {
			extras = append(extras, id)
		}
	}
	slices.Sort(extras)
	for _, id := range extras {
		result = append(result, byID[id])
	}
	return result
}

// FilterSections returns sections not in the disabled list.
func FilterSections(sections []Section, disabled []SectionID) []Section {
	if len(disabled) == 0 {
		return sections
	}
	set := make(map[SectionID]bool, len(disabled))
	for _, id := range disabled {
		set[id] = true
	}
	result := make([]Section, 0, len(sections))
	for _, s := range sections {
		if !set[s.ID] {
			result = append(result, s)
		}
	}
	return result
}

// SectionsToString joins section content with double newlines.
func SectionsToString(sections []Section) string {
	parts := make([]string, len(sections))
	for i, s := range sections {
		parts[i] = strings.TrimSpace(s.Content)
	}
	return strings.Join(parts, "\n\n")
}
