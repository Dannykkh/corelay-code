package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Feedback records user evaluation of a response.
type Feedback struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	MessageID int       `json:"messageId"` // index in session
	Rating    string    `json:"rating"`    // "up", "down"
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Timestamp time.Time `json:"timestamp"`
}

// FeedbackStats summarizes feedback.
type FeedbackStats struct {
	Total      int                   `json:"total"`
	ThumbsUp   int                   `json:"thumbsUp"`
	ThumbsDown int                   `json:"thumbsDown"`
	Score      float64               `json:"score"` // 0.0 - 1.0
	ByModel    map[string]ModelScore `json:"byModel"`
}

// ModelScore is per-model feedback.
type ModelScore struct {
	Up    int     `json:"up"`
	Down  int     `json:"down"`
	Score float64 `json:"score"`
}

// FeedbackStore manages user feedback.
type FeedbackStore struct {
	mu    sync.RWMutex
	items []Feedback
	dir   string
}

func NewFeedbackStore(baseDir string) *FeedbackStore {
	dir := filepath.Join(baseDir, "feedback")
	os.MkdirAll(dir, 0755)
	fs := &FeedbackStore{
		items: make([]Feedback, 0, feedbackMemoryLimit),
		dir:   dir,
	}
	fs.load()
	return fs
}

// Add records a feedback entry.
func (f *FeedbackStore) Add(fb Feedback) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fb.Timestamp = time.Now()
	fb.ID = fb.Timestamp.Format("20060102-150405")
	f.items = append(f.items, fb)
	f.items = trimFeedback(f.items)
	f.save()
}

// Stats computes feedback summary.
func (f *FeedbackStore) Stats() FeedbackStats {
	f.mu.RLock()
	defer f.mu.RUnlock()

	stats := FeedbackStats{
		ByModel: make(map[string]ModelScore),
	}

	for _, fb := range f.items {
		stats.Total++
		if fb.Rating == "up" {
			stats.ThumbsUp++
		} else {
			stats.ThumbsDown++
		}

		ms := stats.ByModel[fb.Model]
		if fb.Rating == "up" {
			ms.Up++
		} else {
			ms.Down++
		}
		if ms.Up+ms.Down > 0 {
			ms.Score = float64(ms.Up) / float64(ms.Up+ms.Down)
		}
		stats.ByModel[fb.Model] = ms
	}

	if stats.Total > 0 {
		stats.Score = float64(stats.ThumbsUp) / float64(stats.Total)
	}

	return stats
}

// Recent returns recent feedback entries.
func (f *FeedbackStore) Recent(limit int) []Feedback {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if limit <= 0 || limit > len(f.items) {
		limit = len(f.items)
	}
	start := len(f.items) - limit
	result := make([]Feedback, limit)
	copy(result, f.items[start:])
	return result
}

func (f *FeedbackStore) save() {
	path := filepath.Join(f.dir, "feedback.json")
	data, _ := json.MarshalIndent(f.items, "", "  ")
	os.WriteFile(path, data, 0644)
}

func (f *FeedbackStore) load() {
	path := filepath.Join(f.dir, "feedback.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if json.Unmarshal(data, &f.items) == nil {
		f.items = trimFeedback(f.items)
	}
}
