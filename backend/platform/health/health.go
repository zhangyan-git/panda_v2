package health

import "sync/atomic"

type Status struct {
	Live  bool
	Ready bool
}
type State struct {
	live  atomic.Bool
	ready atomic.Bool
}

func New() *State                { s := &State{}; s.live.Store(true); return s }
func (s *State) SetReady(v bool) { s.ready.Store(v) }
func (s *State) Status() Status  { return Status{Live: s.live.Load(), Ready: s.ready.Load()} }
func (s *State) Stop()           { s.live.Store(false); s.ready.Store(false) }
