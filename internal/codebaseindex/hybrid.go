package codebaseindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const documentationSearchMaxFileSize = 2 * 1024 * 1024

type documentationCandidate struct {
	result SearchResult
	score  float64
}

func (r *Reader) SupplementRelatedDefinitions(ctx context.Context, projectRoot, query, pathPrefix string, results []SearchResult, limit int) ([]SearchResult, error) {
	if limit <= 0 || r.catalog == nil || len(r.catalog.NativeFiles) == 0 {
		return results, nil
	}
	callCounts := make(map[string]int)
	for _, result := range results[:min(len(results), 20)] {
		if result.FacetCoverage != 0 && result.FacetCoverage < 0.2 {
			continue
		}
		for symbol := range calledSearchSymbols(result.Chunk.Content) {
			callCounts[symbol]++
		}
	}
	if len(callCounts) == 0 {
		return results, nil
	}
	queryTerms := significantSearchTerms(query)
	prefix := normalizeIndexPath(pathPrefix)
	existing := make(map[string]struct{})
	for _, result := range results {
		existing[result.Chunk.Path+"::"+searchSymbolBase(result.Symbol)] = struct{}{}
	}
	candidates := make([]documentationCandidate, 0)
	var scannedBytes int64
	for _, file := range r.catalog.NativeFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := normalizeIndexPath(file.Path)
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		if documentationSearchExtension(path) || file.Size > documentationSearchMaxFileSize || scannedBytes+file.Size > 64*1024*1024 {
			continue
		}
		content, err := readNativeProjectFile(projectRoot, file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scannedBytes += file.Size
		lines := strings.Split(content, "\n")
		for index, line := range lines {
			for symbol, callers := range callCounts {
				if !searchDefinitionLine(line, symbol) {
					continue
				}
				key := path + "::" + symbol
				if _, ok := existing[key]; ok {
					continue
				}
				start := max(0, index-2)
				end := min(len(lines), index+80)
				excerpt := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
				terms := significantSearchTerms(path + " " + symbol + " " + excerpt)
				coverage := termCoverage(queryTerms, terms)
				if coverage < 0.2 {
					continue
				}
				score := 0.25 + coverage*0.30 + min(float64(callers)*0.03, 0.12)
				candidates = append(candidates, documentationCandidate{
					result: SearchResult{Chunk: Chunk{ProjectRoot: projectRoot, Path: path, Content: excerpt, StartLine: start + 1, EndLine: end}, Score: score, Symbol: symbol},
					score:  score,
				})
				existing[key] = struct{}{}
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return betterSearchResult(candidates[i].result, candidates[j].result)
	})
	for _, candidate := range candidates[:min(len(candidates), max(5, limit))] {
		results = append(results, candidate.result)
	}
	return results, nil
}

func searchDefinitionLine(line, symbol string) bool {
	line = strings.TrimSpace(strings.ToLower(line))
	call := symbol + "("
	if !strings.Contains(strings.ReplaceAll(line, " ", ""), call) {
		return false
	}
	if strings.Contains(line, "func ") || strings.Contains(line, "function ") || strings.Contains(line, "fn ") || strings.HasPrefix(line, "def ") {
		return true
	}
	for _, prefix := range []string{"public ", "private ", "protected ", "static ", "async ", "export "} {
		line = strings.TrimPrefix(line, prefix)
	}
	return strings.HasPrefix(strings.ReplaceAll(line, " ", ""), call) && (strings.Contains(line, "{") || strings.HasSuffix(line, ":"))
}

func (r *Reader) SupplementDocumentation(ctx context.Context, projectRoot, query, pathPrefix string, results []SearchResult, limit int) ([]SearchResult, error) {
	if limit <= 0 || r.catalog == nil || len(r.catalog.NativeFiles) == 0 {
		return results, nil
	}
	queryTerms := significantSearchTerms(query)
	if len(queryTerms) == 0 {
		return results, nil
	}
	existing := make(map[string][]Chunk)
	for _, result := range results {
		existing[result.Chunk.Path] = append(existing[result.Chunk.Path], result.Chunk)
	}
	candidates := make([]documentationCandidate, 0)
	prefix := normalizeIndexPath(pathPrefix)
	for _, file := range r.catalog.NativeFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := normalizeIndexPath(file.Path)
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		if !documentationSearchExtension(path) || file.Size > documentationSearchMaxFileSize {
			continue
		}
		content, err := readNativeProjectFile(projectRoot, file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, chunk := range documentationSections(projectRoot, path, content) {
			if documentationChunkPresent(existing[path], chunk) {
				continue
			}
			terms := significantSearchTerms(path + " " + chunk.Content)
			coverage := termCoverage(queryTerms, terms)
			if coverage < 0.35 {
				continue
			}
			candidates = append(candidates, documentationCandidate{
				result: SearchResult{Chunk: chunk, Score: 0.25 + coverage*0.35},
				score:  coverage,
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return betterSearchResult(candidates[i].result, candidates[j].result)
	})
	for _, candidate := range candidates[:min(len(candidates), max(3, limit))] {
		results = append(results, candidate.result)
	}
	return results, nil
}

func documentationSearchExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".mdx", ".rst", ".txt":
		return true
	default:
		return false
	}
}

func documentationSections(projectRoot, path, content string) []Chunk {
	lines := strings.Split(content, "\n")
	starts := []int{0}
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if index > 0 && strings.HasPrefix(trimmed, "#") {
			starts = append(starts, index)
		}
	}
	starts = append(starts, len(lines))
	chunks := make([]Chunk, 0, len(starts))
	chunkIndex := 0
	for index := 0; index+1 < len(starts); index++ {
		sectionStart := starts[index]
		sectionEnd := starts[index+1]
		for start := sectionStart; start < sectionEnd; start += 120 {
			end := min(start+120, sectionEnd)
			value := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
			if value == "" {
				continue
			}
			chunks = append(chunks, Chunk{
				ProjectRoot: projectRoot,
				Path:        path,
				ChunkIndex:  chunkIndex,
				Content:     value,
				StartLine:   start + 1,
				EndLine:     end,
			})
			chunkIndex++
		}
	}
	return chunks
}

func documentationChunkPresent(existing []Chunk, candidate Chunk) bool {
	for _, chunk := range existing {
		if chunksOverlap(chunk, candidate) {
			return true
		}
	}
	return false
}
