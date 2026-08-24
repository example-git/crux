package codebaseindex

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

type ProjectFilters struct {
	IncludePaths []string
	ExcludePaths []string
}

func NormalizeProjectFilters(filters ProjectFilters) ProjectFilters {
	return ProjectFilters{
		IncludePaths: normalizeFilterPaths(filters.IncludePaths),
		ExcludePaths: normalizeFilterPaths(filters.ExcludePaths),
	}
}

func normalizeFilterPaths(paths []string) []string {
	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSuffix(normalizeIndexPath(path), "/")
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		normalized = append(normalized, path)
	}
	sort.Strings(normalized)
	return normalized
}

func filterDigest(filters ProjectFilters) string {
	filters = NormalizeProjectFilters(filters)
	if len(filters.IncludePaths) == 0 && len(filters.ExcludePaths) == 0 {
		return ""
	}
	hash := sha256.New()
	for _, path := range filters.IncludePaths {
		hash.Write([]byte("include\x00" + path + "\x00"))
	}
	for _, path := range filters.ExcludePaths {
		hash.Write([]byte("exclude\x00" + path + "\x00"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func projectPathIncluded(path string, filters ProjectFilters) bool {
	path = normalizeIndexPath(path)
	included := len(filters.IncludePaths) == 0
	for _, prefix := range filters.IncludePaths {
		if indexPathMatchesPrefix(path, prefix) {
			included = true
			break
		}
	}
	if !included {
		return false
	}
	for _, prefix := range filters.ExcludePaths {
		if indexPathMatchesPrefix(path, prefix) {
			return false
		}
	}
	return true
}

func indexPathMatchesPrefix(path, prefix string) bool {
	path = normalizeIndexPath(path)
	prefix = normalizeIndexPath(prefix)
	if prefix == "" {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
