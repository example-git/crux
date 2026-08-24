package styles

import (
	"image/color"

	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/example-git/crux/internal/ui/brand"
)

// ThemeKeyForProvider returns a stable identifier for the theme
// associated with the given provider ID. Providers that share a theme
// yield the same key, so callers can cheaply detect when switching
// providers would not actually change the active theme and skip the
// expensive style rebuild. This is the single source of truth for the
// provider-to-theme mapping; [ThemeForProvider] builds on it.
func ThemeKeyForProvider(providerID string) string {
	if providerID != "" {
		return providerID
	}
	return "default"
}

// ThemeForProvider returns the Styles associated with the given provider
// ID. Providers with a known brand get the default theme re-accented with
// their brand gradient; unknown or empty provider IDs yield the default
// Crux-accented Charmtone Pantera theme.
func ThemeForProvider(providerID string) Styles {
	s := CharmtonePantera()
	if b := brand.ForProvider(providerID); b != nil {
		ApplyBrandAccents(&s, b.GradA, b.GradB, b.Accent)
	}
	return s
}

// CharmtonePantera returns the Charmtone dark theme. It's the default style
// for the UI.
func CharmtonePantera() Styles {
	s := quickStyle(quickStyleOpts{
		primary:   charmtone.Charple,
		secondary: charmtone.Dolly,
		accent:    charmtone.Bok,
		keyword:   charmtone.Blush,

		fgBase:       charmtone.Sash,
		fgMoreSubtle: charmtone.Squid,
		fgSubtle:     charmtone.Smoke,
		fgMostSubtle: charmtone.Oyster,

		onPrimary: charmtone.Butter,

		bgBase:         charmtone.Pepper,
		bgLeastVisible: charmtone.BBQ,
		bgLessVisible:  charmtone.Char,
		bgMostVisible:  charmtone.Iron,

		separator: charmtone.Char,

		destructive:       charmtone.Coral,
		error:             charmtone.Sriracha,
		warningSubtle:     charmtone.Zest,
		warning:           charmtone.Mustard,
		attention:         charmtone.Tang,
		busy:              charmtone.Citron,
		info:              charmtone.Malibu,
		infoMoreSubtle:    charmtone.Sardine,
		infoMostSubtle:    charmtone.Damson,
		success:           charmtone.Julep,
		successMoreSubtle: charmtone.Bok,
		successMostSubtle: charmtone.Guac,

		// ANSI 16-color palette for remapping raw terminal output
		// (e.g. bang-mode shell commands) onto legible Charmtone colors.
		ansiBlack:   charmtone.BBQ,
		ansiRed:     charmtone.Coral,
		ansiGreen:   charmtone.Guac,
		ansiYellow:  charmtone.Mustard,
		ansiBlue:    charmtone.Charple,
		ansiMagenta: charmtone.Dolly,
		ansiCyan:    charmtone.Malibu,
		ansiWhite:   charmtone.Smoke,

		ansiBrightBlack:   charmtone.Iron,
		ansiBrightRed:     charmtone.Tuna,
		ansiBrightGreen:   charmtone.Julep,
		ansiBrightYellow:  charmtone.Zest,
		ansiBrightBlue:    charmtone.Guppy,
		ansiBrightMagenta: charmtone.Blush,
		ansiBrightCyan:    charmtone.Sardine,
		ansiBrightWhite:   charmtone.Salt,
	})

	// Bang ! prompt overrides - use Salt/Hazy/Larple colors.
	s.Editor.PromptBangIconFocused = s.Editor.PromptBangIconFocused.
		Foreground(charmtone.Salt).
		Background(charmtone.Hazy)
	s.Editor.PromptBangDotsFocused = s.Editor.PromptBangDotsFocused.
		Foreground(charmtone.Hazy)
	s.Editor.PromptBangDotsBlurred = s.Editor.PromptBangDotsBlurred.
		Foreground(charmtone.Larple)

	// Shell bar/prompt overrides - use Charple/Iron/Hazy colors.
	s.Messages.ShellBarFocused = s.Messages.ShellBarFocused.
		BorderForeground(charmtone.Charple)
	s.Messages.ShellBarBlurred = s.Messages.ShellBarBlurred.
		BorderForeground(charmtone.Iron)
	s.Messages.ShellPrompt = s.Messages.ShellPrompt.
		Foreground(charmtone.Hazy)
	s.Messages.ShellPromptBlurred = s.Messages.ShellPromptBlurred.
		Foreground(charmtone.Hazy)

	// Default Crux branding: dialogs and menus follow the logo gradient
	// rather than the inherited Charmtone purple primary.
	ApplyBrandAccents(&s, s.Logo.TitleColorA, s.Logo.TitleColorB, s.Logo.TitleColorA)

	return s
}

// ApplyBrandAccents recolors the accent surfaces used by dialogs and menus
// (titles, headers, selected rows, frames, completions, and text selection)
// so they follow the active brand gradient instead of the theme's purple
// primary. gradA is used for accent text, gradA/gradB feed title gradients,
// and accent drives selection backgrounds and frame borders.
func ApplyBrandAccents(s *Styles, gradA, gradB, accent color.Color) {
	onAccent := contrastFg(accent)

	s.Dialog.Title = s.Dialog.Title.Foreground(gradA)
	s.Dialog.TitleText = s.Dialog.TitleText.Foreground(gradA)
	s.Dialog.TitleGradFromColor = gradA
	s.Dialog.TitleGradToColor = gradB
	s.Dialog.PrimaryText = s.Dialog.PrimaryText.Foreground(gradA)
	s.Dialog.SelectedItem = s.Dialog.SelectedItem.Background(accent).Foreground(onAccent)
	s.Dialog.ScrollbarThumb = s.Dialog.ScrollbarThumb.Foreground(gradB)
	s.Dialog.View = s.Dialog.View.BorderForeground(gradB)
	s.Dialog.Quit.Frame = s.Dialog.Quit.Frame.BorderForeground(gradB)
	s.Dialog.Arguments.InputRequiredMarkFocused = s.Dialog.Arguments.InputRequiredMarkFocused.Foreground(gradA)

	s.WorkingGradFromColor = gradA
	s.WorkingGradToColor = gradB

	s.Completions.Focused = s.Completions.Focused.Background(accent).Foreground(onAccent)
	s.TextSelection = s.TextSelection.Background(accent).Foreground(onAccent)
}

// contrastFg picks a legible foreground for text rendered on the given
// background color: dark ink on light accents, light ink on dark accents.
func contrastFg(background color.Color) color.Color {
	r, g, b, _ := background.RGBA()
	// Perceived luminance weights in 16-bit channel space.
	luminance := (299*r + 587*g + 114*b) / 1000
	if luminance > 0x7FFF {
		return charmtone.Pepper
	}
	return charmtone.Butter
}
