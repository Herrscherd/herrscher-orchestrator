package orchestrator

import "sync"

// The Extractor registry lets the *closed* part of the moat plug its curation
// heuristics into this *open* plugin the same way plugins plug into the host: a
// blank import runs an init() that calls RegisterExtractor, and register.go looks
// the extractor up by name at construction time. This package ships no extractor,
// so an unconfigured host keeps the plain Curator behaviour.
var (
	extractorsMu sync.RWMutex
	extractors   = map[string]Extractor{}
)

// RegisterExtractor registers e under name (typically from an init()). A second
// registration under the same name overwrites the first — last writer wins, as
// for any plugin registry.
func RegisterExtractor(name string, e Extractor) {
	extractorsMu.Lock()
	defer extractorsMu.Unlock()
	extractors[name] = e
}

// lookupExtractor returns the extractor registered under name, or nil when none
// is registered (an empty name included).
func lookupExtractor(name string) Extractor {
	extractorsMu.RLock()
	defer extractorsMu.RUnlock()
	return extractors[name]
}
