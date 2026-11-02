package orchestrator

import "sync"

var (
	extractorsMu sync.RWMutex
	extractors   = make(map[string]Extractor)
)

// RegisterExtractor registers e under name (typically from an init()). A second
// registration under the same name overwrites the first; last writer wins, as
// for any plugin registry.
func RegisterExtractor(name string, e Extractor) {
	extractorsMu.Lock()
	defer extractorsMu.Unlock()
	extractors[name] = e
}

func lookupExtractor(name string) Extractor {
	extractorsMu.RLock()
	defer extractorsMu.RUnlock()
	return extractors[name]
}
