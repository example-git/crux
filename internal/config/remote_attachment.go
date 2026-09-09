package config

// WithRemoteAuthorityAdmission atomically checks an accepted client authority
// and admits a short, non-blocking claim mutation. Runtime publication takes
// the same lock, so an old tuple cannot acquire a new claim after replacement.
// admit must not call back into ConfigStore or perform IO.
func (s *ConfigStore) WithRemoteAuthorityAdmission(expected RemoteAuthority, admit func() error) error {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if err := s.RuntimeRevocation(); err != nil {
		return err
	}
	if s.clientRuntime == nil {
		return ErrRemoteRuntimeRevision
	}
	current := s.clientRuntime.authority
	if current.Mode != expected.Mode || current.Principal != expected.Principal || current.Revision != expected.Revision || current.Digest != expected.Digest {
		return ErrRemoteRuntimeRevision
	}
	return admit()
}
