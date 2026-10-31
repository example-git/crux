package localaddon

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

type codexNativeModel struct {
	provider string
	model    string
}

func (workspace *codexNativeWorkspace) modelID(selection config.SelectedModel) string {
	workspace.modelIDsMu.RLock()
	defer workspace.modelIDsMu.RUnlock()
	if id := workspace.modelIDs[codexNativeModel{selection.Provider, selection.Model}]; id != "" {
		return id
	}
	return codexModelID(selection.Provider, selection.Model)
}

func (workspace *codexNativeWorkspace) registerModelIDs(surfaces []providerregistry.Surface) {
	workspace.modelIDsMu.Lock()
	defer workspace.modelIDsMu.Unlock()
	if workspace.modelIDs == nil {
		workspace.modelIDs = make(map[codexNativeModel]string)
	}
	used := make(map[string]codexNativeModel, len(workspace.modelIDs))
	for key, id := range workspace.modelIDs {
		used[id] = key
	}
	keys := make([]codexNativeModel, 0)
	seen := make(map[codexNativeModel]bool)
	counts := make(map[string]int)
	for _, surface := range surfaces {
		for _, model := range surface.Models {
			key := codexNativeModel{surface.ID, model.ID}
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
				counts[codexModelID(key.provider, key.model)]++
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].model < keys[j].model
	})
	for _, key := range keys {
		if workspace.modelIDs[key] != "" {
			continue
		}
		base := codexModelID(key.provider, key.model)
		id := base
		_, occupied := used[id]
		if counts[base] > 1 || occupied || id == "" {
			digest := sha256.Sum256([]byte(key.provider + "\x00" + key.model))
			id = fmt.Sprintf("%s-%x", base, digest[:8])
			for suffix := 1; ; suffix++ {
				if _, occupied := used[id]; !occupied {
					break
				}
				id = fmt.Sprintf("%s-%x-%d", base, digest[:8], suffix)
			}
		}
		workspace.modelIDs[key] = id
		used[id] = key
	}
}
