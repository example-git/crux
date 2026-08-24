package main

// This is an example for testing logo treatments. Do not remove.

import (
	"fmt"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/example-git/crux/internal/ui/logo"
	"github.com/example-git/crux/internal/ui/styles"
)

func main() {
	w, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not get terminal size: %s", err)
	}

	s := styles.CharmtonePantera()
	opts := logo.Opts{
		FieldColor:   s.Logo.FieldColor,
		TitleColorA:  s.Logo.TitleColorA,
		TitleColorB:  s.Logo.TitleColorB,
		VersionColor: s.Logo.VersionColor,
		Width:        w,
	}

	lipgloss.Println(logo.Render(s.Logo.GradCanvas, "v1.0.0", true, opts))
	lipgloss.Println(logo.Render(s.Logo.GradCanvas, "v1.0.0", false, opts))
}
