package localaddon

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/example-git/crux/internal/proto"
)

const codexFuzzyFileLimit = 50

type codexFuzzyFile struct {
	Root      string   `json:"root"`
	Path      string   `json:"path"`
	MatchType string   `json:"match_type"`
	FileName  string   `json:"file_name"`
	Score     uint32   `json:"score"`
	Indices   []uint32 `json:"indices,omitempty"`
}

type codexFuzzySearchResult struct {
	requestID            json.RawMessage
	hasCancellationToken bool
	cancellationToken    string
	tokenGeneration      uint64
	sessionID            string
	sessionGeneration    uint64
	query                string
	files                []codexFuzzyFile
	accepted             bool
	err                  error
}

func codexFuzzyFiles(files []proto.WorkspaceFileSearchMatch) []codexFuzzyFile {
	result := make([]codexFuzzyFile, len(files))
	for index, file := range files {
		result[index] = codexFuzzyFile{
			Root:      file.Root,
			Path:      file.Path,
			MatchType: file.MatchType,
			FileName:  file.FileName,
			Score:     file.Score,
			Indices:   append([]uint32(nil), file.Indices...),
		}
	}
	return result
}

func runCodexFuzzySearch(ctx context.Context, workspace *codexNativeWorkspace, query string, roots []string) ([]string, []codexFuzzyFile, error) {
	response, err := workspace.client.SearchWorkspaceFiles(ctx, workspace.value.ID, proto.WorkspaceFileSearchRequest{
		Query: query,
		Roots: roots,
		Limit: codexFuzzyFileLimit,
	})
	if err != nil {
		return nil, nil, err
	}
	return response.Roots, codexFuzzyFiles(response.Files), nil
}

func completeCodexFuzzySessionUpdate(ctx context.Context, workspace *codexNativeWorkspace, owner, sessionID, updateID string) (bool, string, []codexFuzzyFile, error) {
	response, err := workspace.client.CompleteWorkspaceFileSearchSessionUpdate(ctx, workspace.value.ID, proto.WorkspaceFileSearchSessionCompleteRequest{
		Owner: owner, SessionID: sessionID, UpdateID: updateID,
	})
	if err != nil {
		return false, "", nil, err
	}
	if response.Error != "" {
		return response.Accepted, response.Query, []codexFuzzyFile{}, errors.New(response.Error)
	}
	return response.Accepted, response.Query, codexFuzzyFiles(response.Files), nil
}
