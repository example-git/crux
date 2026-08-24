package logo

import "github.com/MakeNowJust/heredoc"

// This file contains the additional letterforms needed to render provider
// wordmarks (CLAUDE, CODEX, GEMINI, COPILOT) in the same style as the CRUX
// letterform art.

// LetterA renders the letter A in a stylized way.
func LetterA(stretch bool) string {
	// Here's what we're making:
	//
	// ▄▀▀▀▄
	// █▀▀▀█
	// ▀   ▀

	side := heredoc.Doc(`
		▄
		█
		▀
	`)
	middle := heredoc.Doc(`
		▀
		▀
	`)
	return joinLetterform(
		side,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      3,
			minStretch: 7,
			maxStretch: 12,
		}),
		side,
	)
}

// LetterD renders the letter D in a stylized way.
func LetterD(stretch bool) string {
	// Here's what we're making:
	//
	// █▀▀▀▄
	// █   █
	// ▀▀▀▀

	left := heredoc.Doc(`
		█
		█
		▀
	`)
	middle := heredoc.Doc(`
		▀

		▀
	`)
	right := heredoc.Doc(`
		▄
		█
	`)
	return joinLetterform(
		left,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      3,
			minStretch: 7,
			maxStretch: 12,
		}),
		right,
	)
}

// LetterG renders the letter G in a stylized way.
func LetterG(stretch bool) string {
	// Here's what we're making:
	//
	// ▄▀▀▀▀▀
	// █  ▀▀█
	// ▀▀▀▀▀▀

	left := heredoc.Doc(`
		▄
		█
		▀
	`)
	middle := heredoc.Doc(`
		▀

		▀
	`)
	right := heredoc.Doc(`
		▀▀
		▀█
		▀▀
	`)
	return joinLetterform(
		left,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      3,
			minStretch: 7,
			maxStretch: 12,
		}),
		right,
	)
}

// LetterI renders the letter I in a stylized way.
func LetterI(stretch bool) string {
	// Here's what we're making:
	//
	// █
	// █
	// ▀

	return heredoc.Doc(`
		█
		█
		▀
	`)
}

// LetterL renders the letter L in a stylized way.
func LetterL(stretch bool) string {
	// Here's what we're making:
	//
	// █
	// █
	// ▀▀▀▀▀

	left := heredoc.Doc(`
		█
		█
		▀
	`)
	middle := heredoc.Doc(`


		▀
	`)
	return joinLetterform(
		left,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      4,
			minStretch: 7,
			maxStretch: 12,
		}),
	)
}

// LetterM renders the letter M in a stylized way.
func LetterM(stretch bool) string {
	// Here's what we're making:
	//
	// █▀▄▀█
	// █ ▀ █
	// ▀   ▀

	side := heredoc.Doc(`
		█
		█
		▀
	`)
	inside := heredoc.Doc(`
		▀
	`)
	middle := heredoc.Doc(`
		▄
		▀
	`)
	stretchedInside := stretchLetterformPart(inside, letterformProps{
		stretch:    stretch,
		width:      1,
		minStretch: 3,
		maxStretch: 6,
	})
	return joinLetterform(
		side,
		stretchedInside,
		middle,
		stretchedInside,
		side,
	)
}

// LetterN renders the letter N in a stylized way.
func LetterN(stretch bool) string {
	// Here's what we're making:
	//
	// █▀▀▀█
	// █   █
	// ▀   ▀

	side := heredoc.Doc(`
		█
		█
		▀
	`)
	middle := heredoc.Doc(`
		▀
	`)
	return joinLetterform(
		side,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      3,
			minStretch: 7,
			maxStretch: 12,
		}),
		side,
	)
}

// LetterO renders the letter O in a stylized way.
func LetterO(stretch bool) string {
	// Here's what we're making:
	//
	// ▄▀▀▀▄
	// █   █
	// ▀▀▀▀▀

	side := heredoc.Doc(`
		▄
		█
		▀
	`)
	middle := heredoc.Doc(`
		▀

		▀
	`)
	return joinLetterform(
		side,
		stretchLetterformPart(middle, letterformProps{
			stretch:    stretch,
			width:      3,
			minStretch: 7,
			maxStretch: 12,
		}),
		side,
	)
}

// LetterT renders the letter T in a stylized way.
func LetterT(stretch bool) string {
	// Here's what we're making:
	//
	// ▀▀█▀▀
	//   █
	//   ▀

	wing := heredoc.Doc(`
		▀
	`)
	middle := heredoc.Doc(`
		█
		█
		▀
	`)
	stretchedWing := stretchLetterformPart(wing, letterformProps{
		stretch:    stretch,
		width:      2,
		minStretch: 4,
		maxStretch: 6,
	})
	return joinLetterform(
		stretchedWing,
		middle,
		stretchedWing,
	)
}

// LetterX renders the letter X in a stylized way. The shape is the classic
// ANSI-Shadow X compressed onto the 3-row half-block grid, with double-width
// strokes so it matches the weight of the other letterforms.
func LetterX(_ bool) string {
	// Here's what we're making:
	//
	// ▀▄ ▄▀
	//  ▄▀▄
	// ▀   ▀

	return "▀▄ ▄▀\n ▄▀▄ \n▀   ▀"
}

// letterformAlphabet maps runes to letterforms for rendering arbitrary
// wordmarks (e.g. provider names) in the CRUX letterform style.
var letterformAlphabet = map[rune]letterform{
	'A': LetterA,
	'C': LetterC,
	'D': LetterD,
	'E': LetterE,
	'G': LetterG,
	'H': LetterH,
	'I': LetterI,
	'L': LetterL,
	'M': LetterM,
	'N': LetterN,
	'O': LetterO,
	'P': LetterP,
	'R': LetterR,
	'S': LetterSAlt,
	'T': LetterT,
	'U': LetterU,
	'X': LetterX,
	'Y': LetterYAlt,
}

// lettersFor returns the letterforms for a word, and whether every letter
// has one.
func lettersFor(word string) ([]letterform, bool) {
	forms := make([]letterform, 0, len(word))
	for _, r := range word {
		form, ok := letterformAlphabet[r]
		if !ok {
			return nil, false
		}
		forms = append(forms, form)
	}
	return forms, len(forms) > 0
}
