package prompt

import (
	"fmt"
	"testing"
)

func TestSectionsLoad(t *testing.T) {
	sections := AllSections()
	if len(sections) == 0 {
		t.Fatal("no sections loaded")
	}
	for _, s := range sections {
		fmt.Printf("  %-25s %d bytes\n", s.ID, len(s.Content))
	}
	full := SectionsToString(sections)
	fmt.Printf("Total: %d bytes\n", len(full))
	if len(full) < 100 {
		t.Fatalf("sections too short: %d bytes", len(full))
	}
	// Check identity section is first
	if sections[0].ID != "identity" {
		t.Fatalf("expected identity first, got %s", sections[0].ID)
	}
	// Check it contains the token engine line
	if !contains(full, "token engine") {
		t.Fatal("missing token engine identity")
	}
	if !contains(full, "The main agent must prefer codebase_search over search") {
		t.Fatal("missing main-agent codebase-search preference")
	}
	if !contains(full, "the relevant files are indexed") {
		t.Fatal("missing indexed-files condition")
	}
}

func TestExcludedSectionsDoNotLoad(t *testing.T) {
	for _, section := range AllSections() {
		if excludedSections[section.ID] {
			t.Fatalf("excluded section %q was loaded", section.ID)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) > 0 && len(sub) > 0 && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
