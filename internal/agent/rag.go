package agent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RAGResult represents a retrieved context chunk.
type RAGResult struct {
	File    string  `json:"file"`
	Line    int     `json:"line,omitempty"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// RAGSearch searches the project for relevant context based on a query.
// Uses keyword extraction + file search + content grep.
func RAGSearch(workDir string, query string, maxResults int) []RAGResult {
	if maxResults <= 0 {
		maxResults = 5
	}

	keywords := extractKeywords(query)
	if len(keywords) == 0 {
		return nil
	}

	log.Printf("[RAG] Query: %q → keywords: %v", truncateStr(query, 50), keywords)

	// Phase 1: Find relevant files by name
	fileMatches := searchFileNames(workDir, keywords)

	// Phase 2: Search file contents
	contentMatches := searchFileContents(workDir, keywords)

	// Phase 3: Merge and rank
	all := mergeAndRank(fileMatches, contentMatches)

	// Phase 4: Read top results
	var results []RAGResult
	seen := make(map[string]bool)

	for _, match := range all {
		if seen[match.File] {
			continue
		}
		seen[match.File] = true

		content := readFileSnippet(filepath.Join(workDir, match.File), match.Line, 30)
		if content == "" {
			continue
		}

		results = append(results, RAGResult{
			File:    match.File,
			Line:    match.Line,
			Content: content,
			Score:   match.Score,
		})

		if len(results) >= maxResults {
			break
		}
	}

	log.Printf("[RAG] Found %d results for %d keywords", len(results), len(keywords))
	return results
}

// FormatRAGContext formats search results as context for the LLM.
func FormatRAGContext(results []RAGResult) string {
	if len(results) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n--- RELEVANT PROJECT CONTEXT ---\n")
	for _, r := range results {
		sb.WriteString(fmt.Sprintf("\n### %s", r.File))
		if r.Line > 0 {
			sb.WriteString(fmt.Sprintf(" (line %d)", r.Line))
		}
		sb.WriteString("\n```\n")
		sb.WriteString(r.Content)
		sb.WriteString("\n```\n")
	}
	sb.WriteString("--- END CONTEXT ---\n")
	return sb.String()
}

// ── Keyword extraction ──

func extractKeywords(query string) []string {
	// Remove common stop words and extract meaningful terms
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "can": true, "this": true, "that": true,
		"these": true, "those": true, "i": true, "you": true, "he": true,
		"she": true, "it": true, "we": true, "they": true, "me": true,
		"him": true, "her": true, "us": true, "them": true, "my": true,
		"your": true, "his": true, "its": true, "our": true, "their": true,
		"what": true, "which": true, "who": true, "whom": true, "how": true,
		"where": true, "when": true, "why": true, "with": true, "from": true,
		"for": true, "in": true, "on": true, "at": true, "to": true,
		"of": true, "and": true, "or": true, "not": true, "but": true,
		"if": true, "then": true, "than": true, "so": true, "as": true,
		// Korean stop words
		"이": true, "그": true, "저": true, "것": true, "수": true,
		"를": true, "을": true, "에": true, "의": true, "가": true,
		"는": true, "은": true, "로": true, "으로": true, "에서": true,
		"해": true, "해줘": true, "좀": true, "줘": true, "뭐": true,
	}

	words := strings.Fields(strings.ToLower(query))
	var keywords []string
	for _, w := range words {
		// Backticks are in the set because prompts routinely quote commands and
		// identifiers inline ("run `pytest`"); without them the keyword stays
		// "`pytest" and matches nothing on disk.
		w = strings.Trim(w, "?!.,;:'\"()[]{}`") // strip punctuation
		if len(w) < 2 || stopWords[w] {
			continue
		}
		keywords = append(keywords, w)
	}

	// Deduplicate
	seen := make(map[string]bool)
	var unique []string
	for _, k := range keywords {
		if !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}

	// Limit to 10 keywords
	if len(unique) > 10 {
		unique = unique[:10]
	}
	return unique
}

// ── File name search ──

type fileMatch struct {
	File  string
	Line  int
	Score float64
}

func searchFileNames(workDir string, keywords []string) []fileMatch {
	var matches []fileMatch

	skipDirs := map[string]bool{
		"node_modules": true, ".git": true, "dist": true, "build": true,
		"vendor": true, ".next": true, "__pycache__": true, ".venv": true,
		"target": true,
	}

	filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] || strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(workDir, path)
		rel = strings.ReplaceAll(rel, "\\", "/")
		lower := strings.ToLower(rel)

		score := 0.0
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				score += 1.0
				// Bonus for exact filename match
				if strings.Contains(strings.ToLower(info.Name()), kw) {
					score += 0.5
				}
			}
		}

		if score > 0 {
			matches = append(matches, fileMatch{File: rel, Score: score})
		}
		return nil
	})

	sort.Slice(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	if len(matches) > 20 {
		matches = matches[:20]
	}
	return matches
}

// ── Content search (grep-like) ──

func searchFileContents(workDir string, keywords []string) []fileMatch {
	var matches []fileMatch

	codeExts := map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".py": true, ".rs": true, ".java": true, ".cs": true, ".rb": true,
		".md": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true,
		".css": true, ".html": true, ".sql": true, ".sh": true, ".bat": true,
	}

	skipDirs := map[string]bool{
		"node_modules": true, ".git": true, "dist": true, "build": true,
		"vendor": true, ".next": true, "__pycache__": true,
	}

	filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !codeExts[ext] {
			return nil
		}
		if info.Size() > 100*1024 { // skip files > 100KB
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		content := strings.ToLower(string(data))
		lines := strings.Split(string(data), "\n")
		rel, _ := filepath.Rel(workDir, path)
		rel = strings.ReplaceAll(rel, "\\", "/")

		for _, kw := range keywords {
			for lineNum, line := range lines {
				if strings.Contains(strings.ToLower(line), kw) {
					// Count keyword occurrences as score
					score := float64(strings.Count(content, kw)) * 0.1
					matches = append(matches, fileMatch{
						File: rel, Line: lineNum + 1, Score: score,
					})
					break // one match per file per keyword
				}
			}
		}

		return nil
	})

	sort.Slice(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	if len(matches) > 30 {
		matches = matches[:30]
	}
	return matches
}

// ── Merge and rank ──

func mergeAndRank(fileMatches, contentMatches []fileMatch) []fileMatch {
	scoreMap := make(map[string]*fileMatch) // file → best match

	for _, m := range fileMatches {
		if existing, ok := scoreMap[m.File]; ok {
			existing.Score += m.Score
		} else {
			copy := m
			scoreMap[m.File] = &copy
		}
	}

	for _, m := range contentMatches {
		if existing, ok := scoreMap[m.File]; ok {
			existing.Score += m.Score
			if m.Line > 0 && existing.Line == 0 {
				existing.Line = m.Line
			}
		} else {
			copy := m
			scoreMap[m.File] = &copy
		}
	}

	var all []fileMatch
	for _, m := range scoreMap {
		all = append(all, *m)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	return all
}

// ── File reading ──

func readFileSnippet(path string, lineNum int, windowSize int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")

	if lineNum <= 0 {
		// No specific line — return first N lines
		end := windowSize
		if end > len(lines) {
			end = len(lines)
		}
		return strings.Join(lines[:end], "\n")
	}

	// Return window around the matching line
	start := lineNum - windowSize/2
	if start < 0 {
		start = 0
	}
	end := start + windowSize
	if end > len(lines) {
		end = len(lines)
	}

	return strings.Join(lines[start:end], "\n")
}
