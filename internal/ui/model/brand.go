package model

import (
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/brand"
)

type providerBrand = brand.Provider

func (m *UI) brandForProvider(providerID string) *providerBrand {
	if surface, ok := providerregistry.LookupSurface(m.com.Workspace.ProviderSurfaces(), providerID); ok {
		return brand.FromSurface(surface)
	}
	return brand.ForProvider(providerID)
}
