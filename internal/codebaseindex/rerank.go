package codebaseindex

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

type SearchRole string

const (
	SearchRoleDirect       SearchRole = "Direct implementation"
	SearchRoleDelivery     SearchRole = "Delivery/call path"
	SearchRolePersistence  SearchRole = "Persistence"
	SearchRoleRecovery     SearchRole = "Recovery/retry"
	SearchRoleConstruction SearchRole = "Payload/construction"
	SearchRoleStartup      SearchRole = "Startup/call path"
	SearchRoleValidation   SearchRole = "Validation"
	SearchRoleContract     SearchRole = "Contract"
	SearchRoleComparison   SearchRole = "Comparison"
	SearchRoleParallel     SearchRole = "Parallel implementation"
	SearchRoleRelated      SearchRole = "Related behavior"
)

var (
	searchWords = regexp.MustCompile(`[\pL\pN_]+`)
	searchCalls = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

var searchStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {}, "by": {}, "for": {}, "from": {},
	"how": {}, "in": {}, "into": {}, "is": {}, "it": {}, "of": {}, "on": {}, "or": {}, "that": {}, "the": {},
	"this": {}, "to": {}, "what": {}, "when": {}, "where": {}, "which": {}, "with": {},
}

type rerankCandidate struct {
	result            SearchResult
	originalChunk     Chunk
	symbolKey         string
	adjustedScore     float64
	primaryScore      float64
	relationshipScore float64
	fixturePenalty    float64
	terms             map[string]struct{}
	structuralTerms   map[string]struct{}
}

func RerankSearchResults(projectRoot, query string, results []SearchResult, limit int) []SearchResult {
	if limit <= 0 || len(results) == 0 {
		return nil
	}
	queryTerms := significantSearchTerms(query)
	candidateTermFrequency := searchCandidateTermFrequency(results, queryTerms)
	candidates := make([]rerankCandidate, 0, len(results))
	for _, result := range results {
		enriched, symbolKey, structuralContent := enrichSearchResult(projectRoot, result)
		terms := significantSearchTerms(enriched.Chunk.Path + " " + enriched.Symbol + " " + enriched.Chunk.Content)
		structuralTerms := significantSearchTerms(enriched.Chunk.Path + " " + enriched.Symbol + " " + structuralContent)
		coverage := weightedTermCoverage(queryTerms, terms, candidateTermFrequency, len(results))
		exactCoverage := exactTermCoverage(queryTerms, terms)
		symbolCoverage := termCoverage(queryTerms, significantSearchTerms(enriched.Symbol))
		enriched.FacetCoverage = coverage
		enriched.Score = result.Score
		primaryPenalty := primaryEvidencePenalty(enriched, queryTerms)
		fixturePenalty := syntheticFixturePenalty(enriched, queryTerms, terms, structuralTerms)
		adjustedScore := result.Score + exactCoverage*0.06 + coverage*0.16 + symbolCoverage*0.08
		adjustedScore -= primaryPenalty
		adjustedScore -= fixturePenalty
		adjustedScore -= weakFacetPenalty(coverage)
		adjustedScore -= broadExcerptPenalty(enriched, coverage)
		adjustedScore -= lowInformationExcerptPenalty(enriched, coverage)
		adjustedScore -= genericSupportPenalty(enriched, queryTerms, coverage)
		candidates = append(candidates, rerankCandidate{
			result:          enriched,
			originalChunk:   result.Chunk,
			symbolKey:       symbolKey,
			adjustedScore:   adjustedScore,
			primaryScore:    result.Score - primaryPenalty - fixturePenalty,
			fixturePenalty:  fixturePenalty,
			terms:           terms,
			structuralTerms: structuralTerms,
		})
	}

	candidates = deduplicateSearchCandidates(projectRoot, candidates)
	applyCallRelationshipBoost(candidates)
	slices.SortStableFunc(candidates, func(left, right rerankCandidate) int {
		switch {
		case left.adjustedScore > right.adjustedScore:
			return -1
		case left.adjustedScore < right.adjustedScore:
			return 1
		case betterSearchResult(left.result, right.result):
			return -1
		case betterSearchResult(right.result, left.result):
			return 1
		default:
			return 0
		}
	})
	if len(candidates) == 0 {
		return nil
	}

	directIndex := primarySearchCandidate(candidates)
	direct := candidates[directIndex]
	candidates = append([]rerankCandidate{direct}, append(candidates[:directIndex], candidates[directIndex+1:]...)...)
	direct.result.Role = SearchRoleDirect
	for index := 1; index < len(candidates); index++ {
		candidates[index].result.Role = inferSearchRole(candidates[index].result, direct.result, queryTerms)
		candidates[index].adjustedScore += directRelationshipBoost(candidates[index].result, direct.result)
		if candidates[index].fixturePenalty == 0 {
			candidates[index].adjustedScore += searchRoleEvidenceBoost(candidates[index].result, queryTerms)
		}
	}

	selected := make([]rerankCandidate, 0, min(limit, len(candidates)))
	selected = append(selected, direct)
	remaining := append([]rerankCandidate(nil), candidates[1:]...)
	fileCounts := map[string]int{direct.result.Chunk.Path: 1}
	roleCounts := map[SearchRole]int{SearchRoleDirect: 1}
	coveredTerms := matchingSearchTerms(queryTerms, direct.terms)
	for len(selected) < limit && len(remaining) > 0 {
		bestIndex := 0
		bestScore := -3.0
		for index, candidate := range remaining {
			score := candidate.adjustedScore
			uncoveredBoost := uncoveredFacetSelectionBoost(queryTerms, candidate.terms, coveredTerms)
			relationshipBoost := selectedRelationshipBoost(candidate.result, selected)
			if candidate.result.Role == SearchRoleRelated {
				score -= 0.08 + max(0, 0.5-candidate.result.FacetCoverage)*0.4
				if relationshipBoost == 0 && uncoveredBoost < 0.04 {
					score -= 0.12
				}
			}
			score += uncoveredBoost
			score += relationshipBoost
			score -= repeatedCallerSelectionPenalty(candidate.result, selected)
			score -= candidate.fixturePenalty * 0.5
			filePenalty := 0.055
			if relationshipBoost > 0 {
				filePenalty = 0.02
			}
			score -= float64(fileCounts[candidate.result.Chunk.Path]) * filePenalty
			score -= repeatedRoleSelectionPenalty(candidate.result.Role, roleCounts[candidate.result.Role])
			if roleCounts[candidate.result.Role] == 0 {
				score += firstRoleSelectionBoost(candidate.result)
			}
			if score > bestScore {
				bestScore = score
				bestIndex = index
			}
		}
		candidate := remaining[bestIndex]
		selected = append(selected, candidate)
		addMatchingSearchTerms(coveredTerms, queryTerms, candidate.terms)
		fileCounts[candidate.result.Chunk.Path]++
		roleCounts[candidate.result.Role]++
		remaining = slices.Delete(remaining, bestIndex, bestIndex+1)
	}

	output := make([]SearchResult, len(selected))
	for index, candidate := range selected {
		candidate.result = focusSupportingSearchResult(projectRoot, candidate.result, candidate.originalChunk)
		candidate.result.Explanation = searchResultExplanation(queryTerms, candidate.terms, candidate.result)
		output[index] = candidate.result
	}
	return output
}

func matchingSearchTerms(query, candidate map[string]struct{}) map[string]struct{} {
	matched := make(map[string]struct{})
	addMatchingSearchTerms(matched, query, candidate)
	return matched
}

func addMatchingSearchTerms(target, query, candidate map[string]struct{}) {
	for term := range query {
		if searchTermMatches(term, candidate) {
			target[term] = struct{}{}
		}
	}
}

func uncoveredFacetSelectionBoost(query, candidate, covered map[string]struct{}) float64 {
	if len(query) == 0 {
		return 0
	}
	uncovered := 0
	for term := range query {
		if _, ok := covered[term]; ok {
			continue
		}
		if searchTermMatches(term, candidate) {
			uncovered++
		}
	}
	return min(0.16, float64(uncovered)/float64(len(query))*0.45)
}

func selectedRelationshipBoost(candidate SearchResult, selected []rerankCandidate) float64 {
	boost := 0.0
	for _, evidence := range selected {
		if searchResultCallsSymbol(candidate, searchSymbolBase(evidence.result.Symbol)) ||
			searchResultCallsSymbol(evidence.result, searchSymbolBase(candidate.Symbol)) {
			boost += 0.20
		}
	}
	if candidate.Role == SearchRoleValidation && boost > 0 {
		boost += 0.06
	}
	return min(0.36, boost)
}

func repeatedCallerSelectionPenalty(candidate SearchResult, selected []rerankCandidate) float64 {
	if candidate.Role == SearchRoleValidation || candidate.Role == SearchRoleContract {
		return 0
	}
	calls := calledSearchSymbols(candidate.Chunk.Content)
	if len(calls) == 0 {
		return 0
	}
	repeated := 0
	for _, evidence := range selected {
		selectedCalls := calledSearchSymbols(evidence.result.Chunk.Content)
		for symbol := range calls {
			if _, ok := selectedCalls[symbol]; ok {
				repeated++
				break
			}
		}
	}
	return min(0.44, float64(repeated)*0.18)
}

func repeatedRoleSelectionPenalty(role SearchRole, count int) float64 {
	if count == 0 {
		return 0
	}
	weight := 0.035
	switch role {
	case SearchRoleDelivery, SearchRoleRelated:
		weight = 0.09
	case SearchRoleValidation:
		weight = 0.045
	}
	return min(0.21, float64(count)*weight)
}

func applyCallRelationshipBoost(candidates []rerankCandidate) {
	for callerIndex := range candidates {
		caller := &candidates[callerIndex]
		if caller.result.FacetCoverage < 0.2 {
			continue
		}
		calls := calledSearchSymbols(caller.result.Chunk.Content)
		for targetIndex := range candidates {
			if callerIndex == targetIndex {
				continue
			}
			target := &candidates[targetIndex]
			base := searchSymbolBase(target.result.Symbol)
			if base == "" {
				continue
			}
			if _, ok := calls[base]; !ok {
				continue
			}
			boost := min(0.05, 0.02+caller.result.FacetCoverage*0.05)
			target.relationshipScore = min(0.15, target.relationshipScore+boost)
		}
	}
	for index := range candidates {
		candidates[index].adjustedScore += candidates[index].relationshipScore
	}
}

func calledSearchSymbols(content string) map[string]struct{} {
	calls := make(map[string]struct{})
	for _, match := range searchCalls.FindAllStringSubmatch(content, -1) {
		base := strings.ToLower(match[1])
		if len(base) < 5 || searchCallNoise(base) {
			continue
		}
		calls[base] = struct{}{}
	}
	return calls
}

func searchCallNoise(value string) bool {
	switch value {
	case "append", "close", "delete", "error", "len", "make", "new", "panic", "print", "printf", "println", "recover":
		return true
	default:
		return false
	}
}

func primarySearchCandidate(candidates []rerankCandidate) int {
	bestSemanticScore := candidates[0].primaryScore
	for _, candidate := range candidates[1:] {
		bestSemanticScore = max(bestSemanticScore, candidate.primaryScore)
	}
	for index, candidate := range candidates {
		semanticWindow := 0.12
		if candidate.relationshipScore >= 0.06 {
			semanticWindow = 0.22
		}
		if candidate.primaryScore >= bestSemanticScore-semanticWindow {
			return index
		}
	}
	return 0
}

func focusSupportingSearchResult(projectRoot string, result SearchResult, original Chunk) SearchResult {
	if result.Role == SearchRoleDirect || result.Chunk.EndLine-result.Chunk.StartLine+1 <= 120 ||
		original.Path != result.Chunk.Path || original.StartLine < result.Chunk.StartLine || original.EndLine > result.Chunk.EndLine {
		return result
	}
	start := max(result.Chunk.StartLine, original.StartLine-10)
	end := min(result.Chunk.EndLine, original.EndLine+10)
	if end-start+1 > 80 {
		end = start + 79
	}
	content, ok := projectExcerpt(projectRoot, result.Chunk.Path, start, end)
	if !ok {
		return result
	}
	result.Chunk.StartLine = start
	result.Chunk.EndLine = end
	result.Chunk.Content = content
	return result
}

func deduplicateSearchCandidates(projectRoot string, candidates []rerankCandidate) []rerankCandidate {
	output := make([]rerankCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		duplicate := -1
		for index, existing := range output {
			if candidate.symbolKey != "" && candidate.symbolKey == existing.symbolKey {
				duplicate = index
				break
			}
			if candidate.symbolKey == "" && existing.symbolKey == "" && chunksOverlap(candidate.result.Chunk, existing.result.Chunk) {
				duplicate = index
				break
			}
		}
		if duplicate < 0 {
			output = append(output, candidate)
			continue
		}
		existing := output[duplicate]
		if candidate.adjustedScore > existing.adjustedScore {
			output[duplicate] = candidate
		}
		if candidate.symbolKey == "" && existing.symbolKey == "" {
			output[duplicate] = mergeSearchCandidates(projectRoot, output[duplicate], candidate, existing)
		}
	}
	return output
}

func chunksOverlap(left, right Chunk) bool {
	if left.Path != right.Path {
		return false
	}
	overlap := min(left.EndLine, right.EndLine) - max(left.StartLine, right.StartLine) + 1
	shorter := min(left.EndLine-left.StartLine+1, right.EndLine-right.StartLine+1)
	return overlap > 0 && shorter > 0 && float64(overlap)/float64(shorter) >= 0.5
}

func mergeSearchCandidates(projectRoot string, selected, left, right rerankCandidate) rerankCandidate {
	startLine := min(left.result.Chunk.StartLine, right.result.Chunk.StartLine)
	endLine := max(left.result.Chunk.EndLine, right.result.Chunk.EndLine)
	if endLine-startLine+1 > 240 {
		return selected
	}
	content, ok := projectExcerpt(projectRoot, selected.result.Chunk.Path, startLine, endLine)
	if !ok || len(content) > 24*1024 {
		return selected
	}
	selected.result.Chunk.StartLine = startLine
	selected.result.Chunk.EndLine = endLine
	selected.result.Chunk.Content = content
	selected.terms = significantSearchTerms(selected.result.Chunk.Path + " " + content)
	selected.result.FacetCoverage = max(left.result.FacetCoverage, right.result.FacetCoverage)
	return selected
}

func projectExcerpt(projectRoot, relativePath string, startLine, endLine int) (string, bool) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", false
	}
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(relativePath)))
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(content), "\n")
	if startLine < 1 || endLine < startLine || endLine > len(lines) {
		return "", false
	}
	return strings.Join(lines[startLine-1:endLine], "\n"), true
}

func enrichSearchResult(projectRoot string, result SearchResult) (SearchResult, string, string) {
	if filepath.Ext(result.Chunk.Path) != ".go" {
		return result, "", result.Chunk.Content
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return result, "", result.Chunk.Content
	}
	path := filepath.Join(root, filepath.FromSlash(result.Chunk.Path))
	cleanPath := filepath.Clean(path)
	if cleanPath != root && !strings.HasPrefix(cleanPath, root+string(filepath.Separator)) {
		return result, "", result.Chunk.Content
	}
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return result, "", result.Chunk.Content
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, cleanPath, content, 0)
	if err != nil {
		return result, "", result.Chunk.Content
	}
	var selected ast.Node
	var symbol string
	for _, declaration := range file.Decls {
		start := fileSet.Position(declaration.Pos()).Line
		end := fileSet.Position(declaration.End()).Line
		if start > result.Chunk.EndLine || end < result.Chunk.StartLine {
			continue
		}
		if selected == nil || end-start < fileSet.Position(selected.End()).Line-fileSet.Position(selected.Pos()).Line {
			selected = declaration
			symbol = declarationName(declaration)
		}
	}
	if selected == nil || symbol == "" {
		return result, "", result.Chunk.Content
	}
	start := fileSet.Position(selected.Pos()).Line
	end := fileSet.Position(selected.End()).Line
	structuralContent := goNodeWithoutStringLiterals(content, fileSet, selected)
	if end-start+1 <= 240 {
		lines := strings.Split(string(content), "\n")
		if start >= 1 && end <= len(lines) {
			excerpt := strings.Join(lines[start-1:end], "\n")
			if len(excerpt) <= 24*1024 {
				result.Chunk.Content = excerpt
				result.Chunk.StartLine = start
				result.Chunk.EndLine = end
			}
		}
	}
	result.Symbol = symbol
	return result, result.Chunk.Path + "::" + symbol, structuralContent
}

func goNodeWithoutStringLiterals(content []byte, fileSet *token.FileSet, node ast.Node) string {
	start := fileSet.Position(node.Pos()).Offset
	end := fileSet.Position(node.End()).Offset
	if start < 0 || end < start || end > len(content) {
		return ""
	}
	value := append([]byte(nil), content[start:end]...)
	ast.Inspect(node, func(current ast.Node) bool {
		literal, ok := current.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		literalStart := fileSet.Position(literal.Pos()).Offset - start
		literalEnd := fileSet.Position(literal.End()).Offset - start
		literalStart = max(0, literalStart)
		literalEnd = min(len(value), literalEnd)
		for index := literalStart; index < literalEnd; index++ {
			value[index] = ' '
		}
		return true
	})
	return string(value)
}

func declarationName(declaration ast.Decl) string {
	switch declaration := declaration.(type) {
	case *ast.FuncDecl:
		if declaration.Recv == nil || len(declaration.Recv.List) == 0 {
			return declaration.Name.Name
		}
		receiver := receiverName(declaration.Recv.List[0].Type)
		if receiver == "" {
			return declaration.Name.Name
		}
		return receiver + "." + declaration.Name.Name
	case *ast.GenDecl:
		if len(declaration.Specs) == 1 {
			switch spec := declaration.Specs[0].(type) {
			case *ast.TypeSpec:
				return spec.Name.Name
			case *ast.ValueSpec:
				if len(spec.Names) == 1 {
					return spec.Names[0].Name
				}
			}
		}
	}
	return ""
}

func receiverName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return receiverName(expression.X)
	case *ast.IndexExpr:
		return receiverName(expression.X)
	case *ast.IndexListExpr:
		return receiverName(expression.X)
	default:
		return ""
	}
}

func significantSearchTerms(text string) map[string]struct{} {
	terms := make(map[string]struct{})
	for _, raw := range searchWords.FindAllString(text, -1) {
		for _, term := range splitSearchWord(raw) {
			term = strings.ToLower(term)
			if len([]rune(term)) < 3 {
				continue
			}
			if _, stop := searchStopWords[term]; stop {
				continue
			}
			terms[term] = struct{}{}
		}
	}
	return terms
}

func splitSearchWord(word string) []string {
	parts := strings.FieldsFunc(word, func(value rune) bool {
		return value == '_' || value == '-'
	})
	output := make([]string, 0, len(parts))
	for _, part := range parts {
		start := 0
		runes := []rune(part)
		for index := 1; index < len(runes); index++ {
			if unicode.IsUpper(runes[index]) && (unicode.IsLower(runes[index-1]) || unicode.IsDigit(runes[index-1])) {
				output = append(output, string(runes[start:index]))
				start = index
			}
		}
		if start < len(runes) {
			output = append(output, string(runes[start:]))
		}
	}
	if len(output) == 0 {
		return []string{word}
	}
	return output
}

func searchCandidateTermFrequency(results []SearchResult, queryTerms map[string]struct{}) map[string]int {
	frequency := make(map[string]int, len(queryTerms))
	for _, result := range results {
		candidateTerms := significantSearchTerms(result.Chunk.Path + " " + result.Chunk.Content)
		for queryTerm := range queryTerms {
			if searchTermMatches(queryTerm, candidateTerms) {
				frequency[queryTerm]++
			}
		}
	}
	return frequency
}

func weightedTermCoverage(query, candidate map[string]struct{}, frequency map[string]int, candidateCount int) float64 {
	if len(query) == 0 {
		return 0
	}
	var matchedWeight float64
	var totalWeight float64
	for term := range query {
		weight := math.Log(float64(candidateCount+1)/float64(frequency[term]+1)) + 1
		totalWeight += weight
		if searchTermMatches(term, candidate) {
			matchedWeight += weight
		}
	}
	if totalWeight == 0 {
		return 0
	}
	return matchedWeight / totalWeight
}

func searchTermMatches(term string, candidate map[string]struct{}) bool {
	if _, ok := candidate[term]; ok {
		return true
	}
	for candidateTerm := range candidate {
		if strings.HasPrefix(candidateTerm, term) || strings.HasPrefix(term, candidateTerm) {
			return true
		}
		common := 0
		for common < len(term) && common < len(candidateTerm) && term[common] == candidateTerm[common] {
			common++
		}
		shorter := min(len(term), len(candidateTerm))
		if common >= 5 && float64(common)/float64(shorter) >= 0.75 {
			return true
		}
	}
	return false
}

func exactTermCoverage(query, candidate map[string]struct{}) float64 {
	if len(query) == 0 {
		return 0
	}
	matched := 0
	for term := range query {
		if _, ok := candidate[term]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(query))
}

func termCoverage(query, candidate map[string]struct{}) float64 {
	if len(query) == 0 {
		return 0
	}
	matched := 0
	for term := range query {
		if searchTermMatches(term, candidate) {
			matched++
		}
	}
	return float64(matched) / float64(len(query))
}

func inferSearchRole(result, direct SearchResult, queryTerms map[string]struct{}) SearchRole {
	path := strings.ToLower(result.Chunk.Path)
	symbol := strings.ToLower(result.Symbol)
	content := strings.ToLower(result.Chunk.Content)
	directBase := searchSymbolBase(direct.Symbol)
	resultBase := searchSymbolBase(result.Symbol)
	candidateCallsDirect := searchResultCallsSymbol(result, directBase)
	directCallsCandidate := searchResultCallsSymbol(direct, resultBase)

	switch {
	case searchTestPath(path):
		return SearchRoleValidation
	case isContractEvidence(result):
		return SearchRoleContract
	case searchPersistenceResult(path, symbol):
		return SearchRolePersistence
	case strings.Contains(resultBase, "deliver") || strings.Contains(resultBase, "publish") ||
		strings.Contains(resultBase, "inject") || strings.Contains(resultBase, "dispatch") || strings.Contains(resultBase, "subscribe"):
		return SearchRoleDelivery
	case resultBase != "" && resultBase == directBase:
		return SearchRoleParallel
	case strings.Contains(resultBase, "recover") || strings.Contains(resultBase, "retry") ||
		strings.Contains(resultBase, "reload") || strings.Contains(resultBase, "reconnect") || strings.Contains(resultBase, "resume"):
		return SearchRoleRecovery
	case searchTermsContainAny(queryTerms, "restart", "recover", "recovery", "lost") &&
		(strings.Contains(symbol, "shutdown") || strings.Contains(symbol, "graceful") ||
			(strings.Contains(content, "shutdown") && (strings.Contains(content, "killall") || strings.Contains(content, "statuskilled")))):
		return SearchRoleComparison
	case (resultBase == "new" || resultBase == "init" || strings.HasPrefix(resultBase, "initialize")) &&
		(searchTermsContainAny(queryTerms, "startup", "initialize", "initialization", "restart", "boot") || candidateCallsDirect):
		return SearchRoleStartup
	case searchConstructionEvidence(resultBase, content, queryTerms):
		return SearchRoleConstruction
	case candidateCallsDirect || directCallsCandidate:
		return SearchRoleDelivery
	case documentationSearchExtension(path):
		return SearchRoleRelated
	default:
		return SearchRoleRelated
	}
}

func directRelationshipBoost(candidate, direct SearchResult) float64 {
	boost := 0.0
	if searchResultCallsSymbol(candidate, searchSymbolBase(direct.Symbol)) {
		boost += 0.12
	}
	if searchResultCallsSymbol(direct, searchSymbolBase(candidate.Symbol)) {
		boost += 0.12
	}
	return min(0.18, boost)
}

func searchResultCallsSymbol(result SearchResult, symbol string) bool {
	if symbol == "" {
		return false
	}
	_, ok := calledSearchSymbols(result.Chunk.Content)[symbol]
	return ok
}

func searchTestPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") ||
		strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

func isContractEvidence(result SearchResult) bool {
	path := strings.ToLower(result.Chunk.Path)
	if !documentationSearchExtension(path) || result.FacetCoverage < 0.25 {
		return false
	}
	content := strings.ToLower(result.Chunk.Content)
	normative := []string{" must ", " shall ", " requires ", " guarantees ", " invariant", " contract", " exactly once", " becomes ", " transitions ", " rejected", " prohibited"}
	padded := " " + content + " "
	for _, marker := range normative {
		if strings.Contains(padded, marker) {
			return true
		}
	}
	return result.FacetCoverage >= 0.5
}

func searchConstructionEvidence(symbol, content string, queryTerms map[string]struct{}) bool {
	constructorName := strings.HasPrefix(symbol, "new") || strings.HasPrefix(symbol, "build") ||
		strings.HasPrefix(symbol, "create") || strings.HasPrefix(symbol, "encode") || strings.HasPrefix(symbol, "marshal")
	if !constructorName {
		return false
	}
	return searchTermsContainAny(queryTerms, "create", "creation", "construct", "construction", "payload", "encode", "marshal", "message") ||
		strings.Contains(content, "payload") || strings.Contains(content, "marshal") || strings.Contains(content, "encode") ||
		termCoverage(queryTerms, significantSearchTerms(symbol)) >= 0.2
}

func syntheticFixturePenalty(result SearchResult, queryTerms, allTerms, structuralTerms map[string]struct{}) float64 {
	path := strings.ToLower(filepath.ToSlash(result.Chunk.Path))
	if !searchTestPath(path) {
		return 0
	}
	allCoverage := termCoverage(queryTerms, allTerms)
	structuralCoverage := termCoverage(queryTerms, structuralTerms)
	if allCoverage < 0.4 || structuralCoverage >= allCoverage*0.65 {
		return 0
	}
	return min(0.35, 0.15+(allCoverage-structuralCoverage)*0.30)
}

func weakFacetPenalty(coverage float64) float64 {
	switch {
	case coverage < 0.2:
		return 0.16
	case coverage < 0.35:
		return 0.09
	case coverage < 0.5:
		return 0.03
	default:
		return 0
	}
}

func primaryEvidencePenalty(result SearchResult, queryTerms map[string]struct{}) float64 {
	path := strings.ToLower(filepath.ToSlash(result.Chunk.Path))
	switch {
	case searchTestPath(path):
		if searchTermsContainAny(queryTerms, "test", "tests", "testing", "validation", "verify") {
			return 0
		}
		return 0.08
	case documentationSearchExtension(path):
		if searchTermsContainAny(queryTerms, "documentation", "docs", "contract", "spec", "specification") {
			return 0
		}
		return 0.05
	default:
		return 0
	}
}

func broadExcerptPenalty(result SearchResult, coverage float64) float64 {
	lineCount := result.Chunk.EndLine - result.Chunk.StartLine + 1
	if lineCount <= 120 {
		return 0
	}
	penalty := min(float64(lineCount-120)/120*0.14, 0.14)
	if coverage < 0.45 {
		penalty += 0.06
	}
	return penalty
}

func lowInformationExcerptPenalty(result SearchResult, coverage float64) float64 {
	if coverage >= 0.65 {
		return 0
	}
	content := strings.TrimSpace(result.Chunk.Content)
	if content == "" {
		return 0.16
	}
	lines := 0
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	if lines > 5 || len(significantSearchTerms(content)) > 12 {
		return 0
	}
	penalty := 0.10
	lower := strings.ToLower(content)
	if strings.Contains(lower, "return ") &&
		!strings.Contains(lower, " if ") &&
		!strings.Contains(lower, " for ") &&
		!strings.Contains(lower, " switch ") {
		penalty += 0.06
	}
	return penalty
}

func genericSupportPenalty(result SearchResult, queryTerms map[string]struct{}, coverage float64) float64 {
	path := strings.ToLower(filepath.ToSlash(result.Chunk.Path))
	if searchTermsContainAny(queryTerms, "api", "backend", "server", "tool") {
		return 0
	}
	generic := strings.Contains(path, "/agent/tools/") || strings.Contains(path, "/backend/") ||
		strings.HasSuffix(path, "/tasks.go")
	if !generic || coverage >= 0.65 {
		return 0
	}
	return 0.09
}

func searchRoleEvidenceBoost(result SearchResult, queryTerms map[string]struct{}) float64 {
	if result.FacetCoverage < 0.18 {
		return 0
	}
	symbolCoverage := termCoverage(queryTerms, significantSearchTerms(result.Symbol))
	switch result.Role {
	case SearchRoleDelivery:
		return 0.10 + result.FacetCoverage*0.05 + symbolCoverage*0.04
	case SearchRoleRecovery:
		return 0.08 + result.FacetCoverage*0.04 + symbolCoverage*0.04
	case SearchRoleConstruction:
		return 0.09 + result.FacetCoverage*0.04 + symbolCoverage*0.04
	case SearchRoleStartup:
		return 0.08 + symbolCoverage*0.04
	case SearchRoleValidation:
		boost := 0.05 + symbolCoverage*0.10
		terms := significantSearchTerms(result.Symbol + " " + result.Chunk.Content)
		if searchTermsContainAny(terms, "restart", "recover", "recovery") && searchTermsContainAny(terms, "lost") {
			boost += 0.06
		}
		return boost
	case SearchRoleContract:
		return 0.12 + result.FacetCoverage*0.08
	case SearchRoleComparison:
		return 0.09 + symbolCoverage*0.04
	case SearchRoleParallel:
		return 0.08 + symbolCoverage*0.04
	case SearchRolePersistence:
		return 0.06 + symbolCoverage*0.03
	default:
		return 0
	}
}

func firstRoleSelectionBoost(result SearchResult) float64 {
	coverage := result.FacetCoverage
	minimumCoverage := 0.25
	if result.Role == SearchRoleConstruction {
		minimumCoverage = 0.18
	}
	if coverage < minimumCoverage {
		return 0
	}
	var boost float64
	switch result.Role {
	case SearchRoleDelivery:
		boost = 0.13
	case SearchRoleContract:
		boost = 0.12
	case SearchRoleValidation:
		boost = 0.11
	case SearchRoleStartup:
		boost = 0.10
	case SearchRoleComparison:
		boost = 0.09
	case SearchRoleParallel:
		boost = 0.08
	case SearchRoleRecovery:
		boost = 0.08
	case SearchRolePersistence:
		boost = 0.07
	case SearchRoleConstruction:
		boost = 0.09
	default:
		return 0
	}
	return boost * min(1, coverage/0.6)
}

func searchSymbolBase(symbol string) string {
	symbol = strings.ToLower(symbol)
	if index := strings.LastIndex(symbol, "."); index >= 0 {
		return symbol[index+1:]
	}
	return symbol
}

func searchPersistenceResult(path, symbol string) bool {
	return strings.Contains(path, "metadata") || strings.Contains(path, "persist") ||
		strings.Contains(path, "/store") || strings.Contains(path, "/record") ||
		strings.Contains(symbol, "record") || strings.Contains(symbol, "serialize") ||
		strings.Contains(symbol, "deserialize") || strings.Contains(symbol, "metadata")
}

func searchTermsContainAny(terms map[string]struct{}, values ...string) bool {
	for _, value := range values {
		if _, ok := terms[value]; ok {
			return true
		}
	}
	return false
}

func searchRoleExplanation(role SearchRole) string {
	switch role {
	case SearchRoleDirect:
		return "Direct behavioral evidence."
	case SearchRoleDelivery:
		return "Caller or delivery-path evidence."
	case SearchRolePersistence:
		return "Durable state evidence."
	case SearchRoleRecovery:
		return "Recovery or retry evidence."
	case SearchRoleConstruction:
		return "Payload construction evidence."
	case SearchRoleStartup:
		return "Startup wiring evidence."
	case SearchRoleValidation:
		return "Behavioral validation evidence."
	case SearchRoleContract:
		return "Normative contract evidence."
	case SearchRoleComparison:
		return "Contrasting lifecycle evidence."
	case SearchRoleParallel:
		return "Parallel implementation evidence."
	default:
		return "Supporting evidence."
	}
}

func searchResultExplanation(queryTerms, candidateTerms map[string]struct{}, result SearchResult) string {
	matched := make([]string, 0)
	for term := range queryTerms {
		if searchTermMatches(term, candidateTerms) {
			matched = append(matched, term)
		}
	}
	slices.Sort(matched)
	if len(matched) > 5 {
		matched = matched[:5]
	}
	role := searchRoleExplanation(result.Role)
	if len(matched) == 0 {
		if result.Symbol != "" {
			return role + " Enclosing symbol: " + result.Symbol + "."
		}
		return role + " Semantically related supporting result."
	}
	return role + " Matches " + strings.Join(matched, " + ") + "."
}
