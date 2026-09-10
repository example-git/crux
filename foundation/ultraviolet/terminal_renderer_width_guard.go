package uv

import (
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// needsWidthGuard identifies cells for which terminal width or grapheme
// handling can disagree with the composed buffer. This includes combining
// sequences with a nominal width of one (for example emoji presentation).
func needsWidthGuard(cell *Cell) bool {
	if cell == nil || cell.Width == 0 {
		return false
	}
	_, runeBytes := utf8.DecodeRuneInString(cell.Content)
	return cell.Width > 1 || runeBytes < len(cell.Content)
}

// transformWidthGuardedLine keeps a terminal's width disagreement inside the
// affected cell, rather than letting it displace the rest of the row. Moving
// back to the expected column only at the end of a row is too late: a wide
// cluster may already have overwritten an unchanged neighboring pane.
//
// Repaint the entire changed row, including unchanged cells after a cluster,
// because those cells may have been covered by the terminal's wider rendering.
// Disable autowrap during the write so even a cluster at the right edge cannot
// spill onto another row or scroll the screen. Horizontal positioning also
// works in inline mode without assuming the buffer starts at screen row zero.
func (s *TerminalRenderer) transformWidthGuardedLine(newbuf *RenderBuffer, y int) bool {
	oldLine, newLine := s.curbuf.Line(y), newbuf.Line(y)
	var guarded, changed bool
	for x := 0; x < newbuf.Width(); x++ {
		oldCell, newCell := oldLine.At(x), newLine.At(x)
		guarded = guarded || needsWidthGuard(oldCell) || needsWidthGuard(newCell)
		changed = changed || !cellEqual(oldCell, newCell)
	}
	if !guarded {
		return false
	}
	if !changed {
		return true
	}

	s.move(newbuf, 0, y)
	_, _ = s.buf.WriteString(ansi.ResetModeAutoWrap)
	for x := 0; x < newbuf.Width(); x++ {
		cell := newLine.At(x)
		if cell != nil && cell.Width == 0 {
			continue
		}
		s.updatePen(cell)
		width := 1
		if cell == nil {
			_ = s.buf.WriteByte(' ')
		} else {
			width = cell.Width
			if width > 1 {
				// Also handle a terminal rendering the cluster narrower than
				// allocated: continuation columns must not retain old text.
				_, _ = s.buf.WriteString(ansi.EraseCharacter(width))
			}
			_, _ = s.buf.WriteString(cell.Content)
		}
		s.cur.X = min(x+width, newbuf.Width()-1)
		if needsWidthGuard(cell) {
			_, _ = s.buf.WriteString(ansi.CursorHorizontalAbsolute(s.cur.X + 1))
		}
	}
	_, _ = s.buf.WriteString(ansi.SetModeAutoWrap)
	s.atPhantom = false   // Autowrap was disabled even for the final cell.
	s.lineHadWide = false // Each guarded cell has already re-anchored the cursor.
	copy(oldLine, newLine)
	return true
}
