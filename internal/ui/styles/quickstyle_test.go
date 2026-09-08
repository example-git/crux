package styles

import (
	"image/color"
	"testing"

	"github.com/charmbracelet/x/exp/charmtone"
)

func TestDefaultLogoColors(t *testing.T) {
	style := ThemeForProvider("")

	assertColorEqual(t, "field", style.Logo.FieldColor, style.Logo.TitleColorA)
	assertColorEqual(t, "version", style.Logo.VersionColor, style.Logo.TitleColorB)
	assertColorEqual(t, "gradient A", style.Logo.TitleColorA, color.NRGBA{R: 0x39, G: 0xff, B: 0x14, A: 0xff})
	assertColorEqual(t, "gradient B", style.Logo.TitleColorB, color.NRGBA{R: 0xff, G: 0x3b, B: 0x1f, A: 0xff})
}

func TestDefaultButtonColorsUseNeutralContrast(t *testing.T) {
	style := CharmtonePantera()

	assertColorEqual(t, "blurred background", style.Button.Blurred.GetBackground(), style.PanelBackground)
	assertColorEqual(t, "selected background", style.Button.Focused.GetBackground(), charmtone.Sash)
	assertColorEqual(t, "selected foreground", style.Button.Focused.GetForeground(), charmtone.Pepper)
	assertColorEqual(t, "hovered background", style.Button.Hovered.GetBackground(), charmtone.Iron)
}

func TestInputUsesChatBackgroundAndPanelsRemainRecessed(t *testing.T) {
	style := CharmtonePantera()
	background := color.NRGBA{R: 0x18, G: 0x17, B: 0x1D, A: 0xFF}

	assertColorEqual(t, "editor background", style.Editor.Background, style.Background)
	assertColorEqual(t, "sidebar background", style.Sidebar.Background, background)
	assertColorEqual(t, "output panel", style.Tool.SummaryPanel.GetBackground(), background)
	assertColorEqual(t, "content line", style.Tool.ContentLine.GetBackground(), background)
	assertColorEqual(t, "dialog panel", style.Dialog.ContentPanelBg, background)
	assertColorEqual(t, "command panel", style.Dialog.CommandPanel.GetBackground(), background)
	assertColorEqual(t, "permission details", style.Dialog.Permissions.ParamsBg, background)
	assertColorEqual(t, "thinking", style.Messages.ThinkingBox.GetBackground(), style.Background)
	assertColorEqual(t, "focused input", style.Editor.Textarea.Focused.Base.GetBackground(), style.Background)
	assertColorEqual(t, "blurred input", style.Editor.Textarea.Blurred.Base.GetBackground(), style.Background)
	baseR, baseG, baseB, _ := style.Background.RGBA()
	panelR, panelG, panelB, _ := background.RGBA()
	if panelR >= baseR || panelG >= baseG || panelB >= baseB {
		t.Fatal("recessed background must be darker than the main background")
	}
}

func TestNeutralPanelsSharePalette(t *testing.T) {
	style := CharmtonePantera()
	for name, background := range map[string]color.Color{
		"sidebar":     style.Sidebar.Background,
		"summary":     style.Tool.SummaryPanel.GetBackground(),
		"content":     style.Tool.ContentLine.GetBackground(),
		"code":        style.Tool.ContentCodeBg,
		"code line":   style.Tool.ContentCodeLine.GetBackground(),
		"line number": style.Tool.ContentLineNumber.GetBackground(),
		"dialog":      style.Dialog.ContentPanelBg,
		"command":     style.Dialog.CommandPanel.GetBackground(),
		"permissions": style.Dialog.Permissions.ParamsBg,
		"button":      style.Button.Blurred.GetBackground(),
	} {
		assertColorEqual(t, name, background, style.PanelBackground)
	}
}

func assertColorEqual(t *testing.T, name string, got, want color.Color) {
	t.Helper()
	gotR, gotG, gotB, gotA := got.RGBA()
	wantR, wantG, wantB, wantA := want.RGBA()
	if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
		t.Fatalf("%s color = rgba(%d, %d, %d, %d), want rgba(%d, %d, %d, %d)", name, gotR, gotG, gotB, gotA, wantR, wantG, wantB, wantA)
	}
}
