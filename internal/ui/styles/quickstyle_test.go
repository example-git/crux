package styles

import (
	"image/color"
	"testing"
)

func TestDefaultLogoColors(t *testing.T) {
	style := ThemeForProvider("")

	assertColorEqual(t, "field", style.Logo.FieldColor, style.Logo.TitleColorA)
	assertColorEqual(t, "version", style.Logo.VersionColor, style.Logo.TitleColorB)
	assertColorEqual(t, "gradient A", style.Logo.TitleColorA, color.NRGBA{R: 0x39, G: 0xff, B: 0x14, A: 0xff})
	assertColorEqual(t, "gradient B", style.Logo.TitleColorB, color.NRGBA{R: 0xff, G: 0x3b, B: 0x1f, A: 0xff})
}

func assertColorEqual(t *testing.T, name string, got, want color.Color) {
	t.Helper()
	gotR, gotG, gotB, gotA := got.RGBA()
	wantR, wantG, wantB, wantA := want.RGBA()
	if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
		t.Fatalf("%s color = rgba(%d, %d, %d, %d), want rgba(%d, %d, %d, %d)", name, gotR, gotG, gotB, gotA, wantR, wantG, wantB, wantA)
	}
}
