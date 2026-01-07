// Package collector provides metrics collection functionality.
//
// This package implements a source registry that manages multiple
// metrics sources (Prometheus, Direct Pod scraping, EPP).
// Each source maintains its own query registry.
package collector

import (
	"fmt"
	"sync"
)

// SourceRegistry manages multiple metrics sources.
// This replaces the old global query registry approach.
type SourceRegistry struct {
	mu      sync.RWMutex
	sources map[string]MetricsSource
}

// NewSourceRegistry creates a new source registry.
func NewSourceRegistry() *SourceRegistry {
	return &SourceRegistry{
		sources: make(map[string]MetricsSource),
	}
}

// Register adds a metrics source to the registry.
// The source is identified by a unique name (e.g., "prometheus-primary").
func (r *SourceRegistry) Register(name string, source MetricsSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name == "" {
		return fmt.Errorf("source name is required")
	}
	if source == nil {
		return fmt.Errorf("source cannot be nil")
	}

	if _, exists := r.sources[name]; exists {
		return fmt.Errorf("source %q already registered", name)
	}

	r.sources[name] = source
	return nil
}

// MustRegister is like Register but panics on error.
func (r *SourceRegistry) MustRegister(name string, source MetricsSource) {
	if err := r.Register(name, source); err != nil {
		panic(fmt.Sprintf("failed to register source: %v", err))
	}
}

// Get retrieves a registered source by name.
func (r *SourceRegistry) Get(name string) MetricsSource {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.sources[name]
}

// List returns all registered source names.
func (r *SourceRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.sources))
	for name := range r.sources {
		names = append(names, name)
	}
	return names
}

// Unregister removes a source from the registry.
func (r *SourceRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.sources, name)
}
