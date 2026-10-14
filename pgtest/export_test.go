package pgtest

import (
	"slices"
	"testing"
)

// resetForTest stops the RSS guard, closes every server and clears the
// registry, so that the next Pool starts a new server and guard. Tests in
// this package call it at their start and in t.Cleanup, so that the
// process holds at most one server.
func resetForTest(t testing.TB) {
	t.Helper()
	stopGuard()
	registry.mu.Lock()
	servers := registry.servers
	registry.servers = nil
	registry.mu.Unlock()
	for _, s := range servers {
		s.close()
	}
}

// serverFor returns the registry's server for tpl, or nil.
func serverFor(tpl Template) *server {
	key, err := serverKeyOf(tpl)
	if err != nil {
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.servers[key]
}

// closeServers closes every server but leaves it in the registry, so that
// a later Pool for its template gets the closed server.
func closeServers() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, s := range registry.servers {
		s.close()
	}
}

// freeList returns a copy of s's free list, oldest first.
func (s *server) freeList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.free)
}
