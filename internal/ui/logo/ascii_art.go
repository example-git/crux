package logo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed ascii_art_maps.json
var asciiArtMapsJSON []byte

type ASCIIArtMap struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Characters string   `json:"characters"`
	Rows       []string `json:"rows"`
	Overlap    bool     `json:"overlap"`
	Lowercase  bool     `json:"lowercase"`
}

type compiledASCIIArtMap struct {
	ASCIIArtMap
	glyphs    map[rune][]string
	lineCount int
}

var (
	compiledASCIIArtMaps     map[string]compiledASCIIArtMap
	compiledASCIIArtMapsErr  error
	compiledASCIIArtMapsOnce sync.Once
)

func loadASCIIArtMaps() (map[string]compiledASCIIArtMap, error) {
	compiledASCIIArtMapsOnce.Do(func() {
		var maps []ASCIIArtMap
		if err := json.Unmarshal(asciiArtMapsJSON, &maps); err != nil {
			compiledASCIIArtMapsErr = fmt.Errorf("decode ASCII-art maps: %w", err)
			return
		}

		compiledASCIIArtMaps = make(map[string]compiledASCIIArtMap, len(maps))
		for _, asciiArtMap := range maps {
			compiled, err := compileASCIIArtMap(asciiArtMap)
			if err != nil {
				compiledASCIIArtMapsErr = err
				return
			}
			compiledASCIIArtMaps[compiled.ID] = compiled
		}
	})
	return compiledASCIIArtMaps, compiledASCIIArtMapsErr
}

func compileASCIIArtMap(asciiArtMap ASCIIArtMap) (compiledASCIIArtMap, error) {
	characters := []rune(asciiArtMap.Characters)
	if len(characters) == 0 {
		return compiledASCIIArtMap{}, fmt.Errorf("ASCII-art map %q has no characters", asciiArtMap.ID)
	}
	if len(asciiArtMap.Rows)%len(characters) != 0 {
		return compiledASCIIArtMap{}, fmt.Errorf("ASCII-art map %q has incomplete glyph rows", asciiArtMap.ID)
	}

	lineCount := len(asciiArtMap.Rows) / len(characters)
	glyphs := make(map[rune][]string, len(characters))
	for characterIndex, character := range characters {
		glyph := make([]string, lineCount)
		for lineIndex := range lineCount {
			glyph[lineIndex] = asciiArtMap.Rows[lineIndex*len(characters)+characterIndex]
		}
		glyphs[character] = glyph
	}

	return compiledASCIIArtMap{
		ASCIIArtMap: asciiArtMap,
		glyphs:      glyphs,
		lineCount:   lineCount,
	}, nil
}

func ConvertASCIIArt(text, mapID string) (string, error) {
	maps, err := loadASCIIArtMaps()
	if err != nil {
		return "", err
	}
	asciiArtMap, ok := maps[mapID]
	if !ok {
		return "", fmt.Errorf("unknown ASCII-art map %q", mapID)
	}
	return convertASCIIArt(text, asciiArtMap), nil
}

func convertASCIIArt(text string, asciiArtMap compiledASCIIArtMap) string {
	if asciiArtMap.Lowercase {
		text = strings.ToLower(text)
	}

	output := make([]string, asciiArtMap.lineCount)
	fallbackGlyph, ok := asciiArtMap.glyphs[' ']
	if !ok {
		fallbackGlyph = make([]string, asciiArtMap.lineCount)
		for line := range fallbackGlyph {
			fallbackGlyph[line] = " "
		}
	}

	for characterIndex, character := range text {
		glyph, ok := asciiArtMap.glyphs[character]
		if !ok {
			glyph = fallbackGlyph
		}
		for line := range output {
			piece := glyph[line]
			if asciiArtMap.Overlap && characterIndex > 0 {
				if strings.HasSuffix(output[line], " ") {
					output[line] = removeLastRune(output[line])
				} else {
					piece = removeFirstRune(piece)
				}
			}
			output[line] += piece
		}
	}

	return strings.Join(output, "\n")
}

func removeFirstRune(text string) string {
	_, size := utf8.DecodeRuneInString(text)
	return text[size:]
}

func removeLastRune(text string) string {
	_, size := utf8.DecodeLastRuneInString(text)
	return text[:len(text)-size]
}
