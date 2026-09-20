package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxFileReadBytes is a hard upper bound for a single UI file read. The
// handler may return a bounded preview for a larger file, but it never reads
// the complete file into memory.
const maxFileReadBytes int64 = 100_000

const fileProbeBytes = 512

type fileReadResponse struct {
	Path      string      `json:"path"`
	Type      string      `json:"type"`
	Size      int64       `json:"size"`
	Ext       string      `json:"ext,omitempty"`
	Lines     int         `json:"lines"`
	Content   string      `json:"content,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	Entries   []*treeNode `json:"entries,omitempty"`
}

func canonicalWorkspaceRelative(root, fullPath string) (string, error) {
	relative, err := filepath.Rel(root, fullPath)
	if err != nil || filepath.IsAbs(relative) || relative == "." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || relative == ".." {
		return "", errors.New("path outside workspace")
	}
	return filepath.ToSlash(relative), nil
}

func readFilePrefix(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

func isBinaryFileSample(sample []byte) bool {
	return bytes.IndexByte(sample, 0) >= 0 || !utf8.Valid(sample)
}

func isImageExtension(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico":
		return true
	default:
		return false
	}
}

func fileTypeForExtension(ext string) string {
	switch ext {
	case ".md":
		return "markdown"
	case ".json":
		return "json"
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java", ".cs", ".css", ".html", ".yml", ".yaml", ".toml", ".sh", ".ps1":
		return "code"
	default:
		return "text"
	}
}

func previewLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	return bytes.Count(content, []byte{'\n'}) + 1
}
