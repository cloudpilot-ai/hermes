package controller

import "sync"

type StringSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func NewStringSet() *StringSet {
	return &StringSet{m: make(map[string]struct{})}
}

func (s *StringSet) Add(v string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[v]; ok {
		return false
	}
	s.m[v] = struct{}{}
	return true
}

func (s *StringSet) Delete(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, v)
}
