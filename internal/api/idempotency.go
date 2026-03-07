package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// idempotencyCache stores responses keyed by Idempotency-Key header values.
// Used on create endpoints so clients can safely retry after network failures.
type idempotencyCache struct {
	mu      sync.Mutex
	entries map[string]cachedEntry
	ttl     time.Duration
}

type cachedEntry struct {
	statusCode int
	body       []byte
	bodyHash   string
	expiresAt  time.Time
}

func newIdempotencyCache(ttl time.Duration) *idempotencyCache {
	return &idempotencyCache{
		entries: make(map[string]cachedEntry),
		ttl:     ttl,
	}
}

// check returns the cached entry for the given key if it exists and hasn't expired.
func (c *idempotencyCache) check(key string) (cachedEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return cachedEntry{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return cachedEntry{}, false
	}
	return entry, true
}

// store caches a response for the given key.
func (c *idempotencyCache) store(key string, statusCode int, body []byte, bodyHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedEntry{
		statusCode: statusCode,
		body:       body,
		bodyHash:   bodyHash,
		expiresAt:  time.Now().Add(c.ttl),
	}
	// Lazy cleanup when cache grows large.
	if len(c.entries) > 1000 {
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				delete(c.entries, k)
			}
		}
	}
}

// handleIdempotent checks for a cached response matching the given
// Idempotency-Key and body hash. Returns true if it handled the response
// (either replayed a cached response or wrote a 422 mismatch error).
// Returns false if the caller should proceed with normal processing.
func (c *idempotencyCache) handleIdempotent(w http.ResponseWriter, key, bodyHash string) bool {
	if key == "" {
		return false
	}
	cached, ok := c.check(key)
	if !ok {
		return false
	}
	if cached.bodyHash != bodyHash {
		writeError(w, http.StatusUnprocessableEntity, "idempotency_mismatch",
			"Idempotency-Key reused with different request body")
		return true
	}
	// Replay cached response.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(cached.statusCode)
	w.Write(cached.body) //nolint:errcheck // best-effort
	return true
}

// storeResponse caches the JSON-serialized response for later replay.
//
//nolint:unparam // statusCode is 201 today but the cache is status-agnostic by design
func (c *idempotencyCache) storeResponse(key, bodyHash string, statusCode int, v any) {
	if key == "" {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	// Append newline to match json.Encoder.Encode behavior.
	data = append(data, '\n')
	c.store(key, statusCode, data, bodyHash)
}

// hashBody returns a hex-encoded SHA-256 hash of the JSON-marshaled body.
func hashBody(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
