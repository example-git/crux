package providerregistry

import "github.com/example-git/crux/internal/providertransport"

const maxMappedErrorTextRunes = providertransport.MaxMappedErrorTextRunes

func (r Registration) MapError(err error) error {
	return providertransport.MapError(r.Errors, err)
}
