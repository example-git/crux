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

	assertColorEqual(t, "blurred background", style.Button.Blurred.GetBackground(), charmtone.BBQ)
	assertColorEqual(t, "selected background", style.Button.Focused.GetBackground(), charmtone.Sash)
	assertColorEqual(t, "selected foreground", style.Button.Focused.GetForeground(), charmtone.Pepper)
	assertColorEqual(t, "hovered background", style.Button.Hovered.GetBackground(), charmtone.Iron)
}

func assertColorEqual(t *testing.T, name string, got, want color.Color) {
	t.Helper()
	gotR, gotG, gotB, gotA := got.RGBA()
	wantR, wantG, wantB, wantA := want.RGBA()
	if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
		t.Fatalf("%s color = rgba(%d, %d, %d, %d), want rgba(%d, %d, %d, %d)", name, gotR, gotG, gotB, gotA, wantR, wantG, wantB, wantA)
	}
}
