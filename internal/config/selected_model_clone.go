package config

// Clone returns an independent model selection, including its optional values
// and provider options, for work that outlives the initiating UI message.
func (m SelectedModel) Clone() SelectedModel { return cloneSelectedModel(m) }
