package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectInfo describes the detected project type and structure.
type ProjectInfo struct {
	Type      string   `json:"type"`      // "go", "node", "python", "rust", "java", "unknown"
	Name      string   `json:"name"`
	Framework string   `json:"framework"` // "react", "next", "express", "fastapi", "spring", etc.
	FileTree  string   `json:"fileTree"`
	FileCount int      `json:"fileCount"`
	MainFiles []string `json:"mainFiles"` // key entry points
}

// DetectProject analyzes a workspace and returns project info.
func DetectProject(workDir string) ProjectInfo {
	info := ProjectInfo{Type: "unknown", Name: filepath.Base(workDir)}

	// Detect by config files
	if fileExists(filepath.Join(workDir, "go.mod")) {
		info.Type = "go"
		info.MainFiles = findFiles(workDir, "main.go", 3)
	} else if fileExists(filepath.Join(workDir, "package.json")) {
		info.Type = "node"
		info.Framework = detectNodeFramework(workDir)
		info.MainFiles = []string{"package.json"}
	} else if fileExists(filepath.Join(workDir, "pyproject.toml")) || fileExists(filepath.Join(workDir, "requirements.txt")) {
		info.Type = "python"
		info.Framework = detectPythonFramework(workDir)
	} else if fileExists(filepath.Join(workDir, "Cargo.toml")) {
		info.Type = "rust"
	} else if fileExists(filepath.Join(workDir, "pom.xml")) || fileExists(filepath.Join(workDir, "build.gradle")) {
		info.Type = "java"
		if fileExists(filepath.Join(workDir, "build.gradle")) {
			info.Framework = "gradle"
		} else {
			info.Framework = "maven"
		}
	} else if fileExists(filepath.Join(workDir, "*.csproj")) || fileExists(filepath.Join(workDir, "*.sln")) {
		info.Type = "dotnet"
	}

	// Marker files are the strong signal, but plenty of real workspaces are just
	// a folder of sources: a scripts directory, a fixture, an early prototype.
	// Without this fallback the Test tool answers "no test runner configured"
	// for a directory full of test_*.py — and reports that as a success.
	if info.Type == "unknown" {
		info.Type = detectTypeByExtension(workDir)
	}

	// Build file tree (max depth 3, max 100 items)
	info.FileTree = buildFileTree(workDir, 3, 100)
	info.FileCount = countFiles(workDir)

	return info
}

// detectTypeByExtension counts source files directly under workDir and returns
// the language with the most. Only used when no marker file was found.
func detectTypeByExtension(workDir string) string {
	langByExt := map[string]string{
		".py":   "python",
		".go":   "go",
		".rs":   "rust",
		".ts":   "node",
		".tsx":  "node",
		".js":   "node",
		".jsx":  "node",
		".java": "java",
		".cs":   "dotnet",
	}

	entries, err := os.ReadDir(workDir)
	if err != nil {
		return "unknown"
	}

	counts := make(map[string]int)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if lang, ok := langByExt[strings.ToLower(filepath.Ext(entry.Name()))]; ok {
			counts[lang]++
		}
	}

	best, bestCount := "unknown", 0
	for lang, count := range counts {
		if count > bestCount {
			best, bestCount = lang, count
		}
	}
	return best
}

// ProjectContext returns a system prompt section for the detected project.
func (p ProjectInfo) ToPrompt() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n\n## Project: %s (%s", p.Name, p.Type))
	if p.Framework != "" {
		sb.WriteString("/" + p.Framework)
	}
	sb.WriteString(fmt.Sprintf(", %d files)\n", p.FileCount))

	// Type-specific hints
	switch p.Type {
	case "go":
		sb.WriteString("- Use `go build`, `go test`, `go vet` for building/testing\n")
		sb.WriteString("- Imports must be used or removed\n")
	case "node":
		sb.WriteString("- Use `npm run` or `bun run` for scripts\n")
		if p.Framework == "react" || p.Framework == "next" {
			sb.WriteString("- React components in .tsx files\n")
		}
	case "python":
		sb.WriteString("- Use `python -m pytest` for tests, `pip install` for deps\n")
		if p.Framework == "fastapi" {
			sb.WriteString("- FastAPI app, use `uvicorn` to run\n")
		}
	case "rust":
		sb.WriteString("- Use `cargo build`, `cargo test`, `cargo clippy`\n")
	}

	// Compact file tree
	if p.FileTree != "" {
		sb.WriteString("\n### File Structure\n```\n")
		sb.WriteString(p.FileTree)
		sb.WriteString("```\n")
	}

	return sb.String()
}

func detectNodeFramework(workDir string) string {
	data, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return ""
	}
	content := string(data)
	if strings.Contains(content, "\"next\"") {
		return "next"
	}
	if strings.Contains(content, "\"react\"") {
		return "react"
	}
	if strings.Contains(content, "\"express\"") {
		return "express"
	}
	if strings.Contains(content, "\"vue\"") {
		return "vue"
	}
	if strings.Contains(content, "\"svelte\"") {
		return "svelte"
	}
	return "node"
}

func detectPythonFramework(workDir string) string {
	// Check imports in common files
	for _, name := range []string{"main.py", "app.py", "manage.py", "pyproject.toml"} {
		data, err := os.ReadFile(filepath.Join(workDir, name))
		if err != nil {
			continue
		}
		content := string(data)
		if strings.Contains(content, "fastapi") || strings.Contains(content, "FastAPI") {
			return "fastapi"
		}
		if strings.Contains(content, "django") || strings.Contains(content, "Django") {
			return "django"
		}
		if strings.Contains(content, "flask") || strings.Contains(content, "Flask") {
			return "flask"
		}
	}
	return ""
}

func buildFileTree(root string, maxDepth, maxItems int) string {
	var lines []string
	count := 0

	skipDirs := map[string]bool{
		"node_modules": true, ".git": true, "__pycache__": true,
		"dist": true, "build": true, ".next": true, "vendor": true,
		".venv": true, "venv": true, "target": true, ".idea": true,
	}

	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || count >= maxItems {
			return filepath.SkipDir
		}

		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}

		depth := strings.Count(rel, string(os.PathSeparator))
		if depth >= maxDepth {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		name := filepath.Base(path)
		if info.IsDir() && skipDirs[name] {
			return filepath.SkipDir
		}

		indent := strings.Repeat("  ", depth)
		if info.IsDir() {
			lines = append(lines, indent+name+"/")
		} else {
			lines = append(lines, indent+name)
		}
		count++
		return nil
	})

	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func countFiles(root string) int {
	count := 0
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := filepath.Base(path)
		if name == "node_modules" || name == ".git" || name == "vendor" || name == ".venv" {
			return filepath.SkipDir
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	return count
}

func findFiles(root, name string, max int) []string {
	var found []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || len(found) >= max {
			return nil
		}
		if filepath.Base(path) == name {
			rel, _ := filepath.Rel(root, path)
			found = append(found, rel)
		}
		return nil
	})
	return found
}

func fileExists(path string) bool {
	// Handle glob patterns
	if strings.Contains(path, "*") {
		matches, _ := filepath.Glob(path)
		return len(matches) > 0
	}
	_, err := os.Stat(path)
	return err == nil
}
