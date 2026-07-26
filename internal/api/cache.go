package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"
)

// CacheStrategy determines how prompt caching is applied.
type CacheStrategy string

const (
	CacheNone        CacheStrategy = "none"         // no caching
	CacheSystemPrompt CacheStrategy = "system_prompt" // cache system blocks
	CacheToolBased   CacheStrategy = "tool_based"    // cache tools + system
)

// CacheControl represents an Anthropic cache control directive.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral_1h" or "ephemeral_5m"
}

// PromptCacheState tracks cache hits/misses for a session.
type PromptCacheState struct {
	mu            sync.Mutex
	lastHash      string    // hash of system + tools for break detection
	cacheHits     int
	cacheMisses   int
	cacheBreaks   int       // unexpected cache misses
	lastCacheRead int       // tokens read from cache on last request
	lastCacheWrite int      // tokens written to cache on last request
	strategy      CacheStrategy
}

// NewPromptCacheState creates a cache state tracker.
func NewPromptCacheState(strategy CacheStrategy) *PromptCacheState {
	return &PromptCacheState{strategy: strategy}
}

// ComputeHash generates a fingerprint of the cacheable request components.
func (pc *PromptCacheState) ComputeHash(systemPrompt string, toolSchemas string, model string, betas string) string {
	h := sha256.New()
	h.Write([]byte(systemPrompt))
	h.Write([]byte("|"))
	h.Write([]byte(toolSchemas))
	h.Write([]byte("|"))
	h.Write([]byte(model))
	h.Write([]byte("|"))
	h.Write([]byte(betas))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// CheckForBreak compares current hash with previous and detects cache breaks.
func (pc *PromptCacheState) CheckForBreak(currentHash string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.lastHash == "" {
		pc.lastHash = currentHash
		return false // first request, no break
	}

	if pc.lastHash != currentHash {
		pc.cacheBreaks++
		log.Printf("[Cache] Break detected (hash %s → %s). Total breaks: %d",
			pc.lastHash[:8], currentHash[:8], pc.cacheBreaks)
		pc.lastHash = currentHash
		return true
	}

	return false
}

// RecordUsage updates cache hit/miss counts from API response.
func (pc *PromptCacheState) RecordUsage(cacheRead, cacheWrite int) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.lastCacheRead = cacheRead
	pc.lastCacheWrite = cacheWrite

	if cacheRead > 0 {
		pc.cacheHits++
	} else {
		pc.cacheMisses++
	}
}

// Stats returns cache statistics.
func (pc *PromptCacheState) Stats() map[string]interface{} {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	hitRate := 0.0
	total := pc.cacheHits + pc.cacheMisses
	if total > 0 {
		hitRate = float64(pc.cacheHits) / float64(total)
	}

	return map[string]interface{}{
		"strategy":       pc.strategy,
		"hits":           pc.cacheHits,
		"misses":         pc.cacheMisses,
		"breaks":         pc.cacheBreaks,
		"hitRate":         fmt.Sprintf("%.1f%%", hitRate*100),
		"lastCacheRead":  pc.lastCacheRead,
		"lastCacheWrite": pc.lastCacheWrite,
	}
}

// GetCacheControlForSource returns the appropriate TTL based on request source.
func GetCacheControlForSource(source string) *CacheControl {
	switch source {
	case "web", "agent":
		return &CacheControl{Type: "ephemeral_1h"} // long-lived for interactive
	case "compact", "summary":
		return &CacheControl{Type: "ephemeral_1h"} // long-lived for compaction
	default:
		return &CacheControl{Type: "ephemeral_5m"} // short-lived for background
	}
}

// ShouldCache determines if caching should be used for this request.
func ShouldCache(strategy CacheStrategy, model string) bool {
	if strategy == CacheNone {
		return false
	}
	// Most models support caching
	return true
}

// ── Keep-alive for cache warmth ──

// CacheKeepAlive tracks when the last request was made to maintain cache warmth.
type CacheKeepAlive struct {
	mu       sync.Mutex
	lastPing time.Time
	interval time.Duration // default 4 minutes (cache expires at 5m)
}

func NewCacheKeepAlive() *CacheKeepAlive {
	return &CacheKeepAlive{
		interval: 4 * time.Minute,
	}
}

func (ka *CacheKeepAlive) Ping() {
	ka.mu.Lock()
	ka.lastPing = time.Now()
	ka.mu.Unlock()
}

func (ka *CacheKeepAlive) NeedsPing() bool {
	ka.mu.Lock()
	defer ka.mu.Unlock()
	return time.Since(ka.lastPing) > ka.interval
}
