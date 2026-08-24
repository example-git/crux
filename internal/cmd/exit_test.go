package cmd

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestSessionResumeLinesUseLogoGradientColors(t *testing.T) {
	t.Parallel()

	theme := styles.ThemeForProvider("")
	sessionLine, continueLine := sessionResumeLines(theme, "Rebrand Crux", "abc1234")

	require.Equal(t,
		lipgloss.NewStyle().Foreground(theme.Logo.TitleColorA).Render("Session  ")+"Rebrand Crux",
		sessionLine,
	)
	require.Equal(t,
		lipgloss.NewStyle().Foreground(theme.Logo.TitleColorB).Render("Continue ")+"crux -s abc1234",
		continueLine,
	)
}

func TestRandomExitMessageUsesSarcasticMessageSet(t *testing.T) {
	t.Parallel()

	require.NotEmpty(t, exitMessages)
	for _, message := range exitMessages {
		require.NotEmpty(t, message)
	}
	require.Contains(t, exitMessages, randomExitMessage())
}
