package apiserver

import (
	"github.com/YukinaMochizuki/tower/internal/controller"
)

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
