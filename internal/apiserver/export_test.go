package apiserver

import "github.com/YukinaMochizuki/tower/internal/provider"

// ProviderFor is the provider tower calls a target through, for tests.
func (s *Server) ProviderFor(target string) (provider.Provider, error) { return s.providerFor(target) }
