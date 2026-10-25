package apiserver

import (
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// ProviderFor is the provider tower calls a target through, for tests.
func (s *Server) ProviderFor(target string) (provider.Provider, error) { return s.providerFor(target) }

// SetBeforeIdleStop runs fn between a box's idle stop being decided and its
// being written, for tests of what happens between.
func (s *Server) SetBeforeIdleStop(fn func(project, box string)) {
	hook := func(k controller.Key) { fn(k.Project, k.Name) }
	s.beforeIdleStop.Store(&hook)
}

// InUse reports whether a call into the box is under way.
func (s *Server) InUse(project, box string) bool {
	_, _, busy := s.useOf(controller.Key{Project: project, Name: box})
	return busy
}
