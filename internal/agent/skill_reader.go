package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SkillBodyReadRequest identifies one registered skill and one bounded byte
// range. RelativePath is always interpreted beneath that skill's module root.
type SkillBodyReadRequest struct {
	SkillID      string `json:"skill_id"`
	RelativePath string `json:"relative_path,omitempty"`
	Offset       int64  `json:"offset,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type SkillBodyReader func(SkillBodyReadRequest) (SkillBodyChunk, error)

// SkillBodyChunk makes continuation explicit. Offsets are UTF-8 boundaries in
// the verified source file, and the caller must continue until EOF is true.
type SkillBodyChunk struct {
	SkillID      string `json:"skill_id"`
	RelativePath string `json:"relative_path,omitempty"`
	ModuleRoot   string `json:"module_root"`
	Digest       string `json:"digest"`
	Content      string `json:"content"`
	Offset       int64  `json:"offset"`
	NextOffset   int64  `json:"next_offset"`
	TotalBytes   int64  `json:"total_bytes"`
	EOF          bool   `json:"eof"`
}

type skillBodyReadState struct {
	NextOffset int64
	TotalBytes int64
	Digest     string
	Pages      int
	EOF        bool
}

type skillFileSnapshot struct {
	Content    []byte
	Digest     string
	ModuleRoot string
}

func loadSkillFileSnapshot(descriptor SkillDescriptor, relativePath string, maxSnapshotBytes int64, excludedPaths map[string]struct{}, excludedFiles []os.FileInfo) (skillFileSnapshot, error) {
	if strings.TrimSpace(descriptor.Path) == "" {
		return skillFileSnapshot{}, fmt.Errorf("skill descriptor is invalid")
	}
	rootPath := strings.TrimSpace(descriptor.ModuleRoot)
	if rootPath == "" {
		rootPath = filepath.Dir(descriptor.Path)
	}
	root, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return skillFileSnapshot{}, fmt.Errorf("skill module root is unavailable")
	}
	var candidatePath string
	if relativePath == "" {
		candidatePath = descriptor.Path
	} else {
		cleanRelative, pathErr := cleanSkillRelativePath(relativePath)
		if pathErr != nil {
			return skillFileSnapshot{}, pathErr
		}
		candidatePath = filepath.Join(root, cleanRelative)
	}
	resolvedPath, err := filepath.EvalSymlinks(candidatePath)
	if err != nil || !pathIsWithin(root, resolvedPath) {
		return skillFileSnapshot{}, fmt.Errorf("skill resource is unavailable inside its module root")
	}
	if relativePath != "" {
		if _, isSkillBody := excludedPaths[skillResourcePathKey(resolvedPath)]; isSkillBody {
			return skillFileSnapshot{}, fmt.Errorf("registered skill bodies are not available as module resources")
		}
	}
	lexicalInfo, err := os.Lstat(resolvedPath)
	if err != nil || !lexicalInfo.Mode().IsRegular() || lexicalInfo.Size() < 0 || lexicalInfo.Size() > maxSkillFileBytes || lexicalInfo.Size() > maxSnapshotBytes {
		return skillFileSnapshot{}, fmt.Errorf("skill resource is not a supported regular file")
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return skillFileSnapshot{}, fmt.Errorf("skill resource is unavailable inside its module root")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(lexicalInfo, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() > maxSnapshotBytes {
		return skillFileSnapshot{}, fmt.Errorf("skill resource changed while opening")
	}
	if relativePath != "" {
		for _, excludedInfo := range excludedFiles {
			if excludedInfo != nil && os.SameFile(openedInfo, excludedInfo) {
				return skillFileSnapshot{}, fmt.Errorf("registered skill bodies are not available as module resources")
			}
		}
	}
	readLimit := maxSkillFileBytes + 1
	if maxSnapshotBytes+1 < readLimit {
		readLimit = maxSnapshotBytes + 1
	}
	content, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil || int64(len(content)) > maxSkillFileBytes || int64(len(content)) > maxSnapshotBytes || int64(len(content)) != openedInfo.Size() || !utf8.Valid(content) {
		return skillFileSnapshot{}, fmt.Errorf("skill resource is invalid or changed while reading")
	}
	finalInfo, err := file.Stat()
	if err != nil || !os.SameFile(openedInfo, finalInfo) || finalInfo.Size() != int64(len(content)) {
		return skillFileSnapshot{}, fmt.Errorf("skill resource changed while reading")
	}
	digestBytes := sha256.Sum256(content)
	return skillFileSnapshot{
		Content: content, Digest: "sha256:" + hex.EncodeToString(digestBytes[:]),
		ModuleRoot: root,
	}, nil
}

func skillSnapshotChunk(descriptor SkillDescriptor, relativePath string, snapshot skillFileSnapshot, offset int64, limit int) (SkillBodyChunk, error) {
	if offset < 0 {
		return SkillBodyChunk{}, fmt.Errorf("invalid skill continuation offset")
	}
	if limit == 0 {
		limit = defaultSkillBodyReadBytes
	}
	if limit < minSkillBodyReadBytes || limit > maxSkillBodyReadBytes {
		return SkillBodyChunk{}, fmt.Errorf("skill read limit must be between %d and %d bytes", minSkillBodyReadBytes, maxSkillBodyReadBytes)
	}
	content := snapshot.Content
	totalBytes := int64(len(content))
	if offset > totalBytes {
		return SkillBodyChunk{}, fmt.Errorf("invalid skill continuation offset")
	}
	if offset < totalBytes {
		if content[offset]&0xc0 == 0x80 {
			return SkillBodyChunk{}, fmt.Errorf("skill continuation offset is not a UTF-8 boundary")
		}
	}
	end := int(offset) + limit
	if end > len(content) {
		end = len(content)
	}
	chunkLength := end - int(offset)
	if chunkLength > limit {
		chunkLength = limit
	}
	for chunkLength > 0 && !utf8.Valid(content[int(offset):int(offset)+chunkLength]) {
		chunkLength--
	}
	if chunkLength == 0 && offset < totalBytes {
		return SkillBodyChunk{}, fmt.Errorf("skill resource has an invalid UTF-8 sequence")
	}
	page := string(content[int(offset) : int(offset)+chunkLength])
	nextOffset := offset + int64(chunkLength)
	if nextOffset < totalBytes && chunkLength == 0 {
		return SkillBodyChunk{}, fmt.Errorf("skill reader could not make progress")
	}
	return SkillBodyChunk{
		SkillID: descriptor.ID, RelativePath: relativePath, ModuleRoot: snapshot.ModuleRoot,
		Digest: snapshot.Digest, Content: page, Offset: offset,
		NextOffset: nextOffset, TotalBytes: totalBytes, EOF: nextOffset == totalBytes,
	}, nil
}

func cleanSkillRelativePath(path string) (string, error) {
	if len(path) > maxSkillRelativePathBytes {
		return "", fmt.Errorf("skill resource path exceeds %d UTF-8 bytes", maxSkillRelativePathBytes)
	}
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	if path == "" || strings.ContainsRune(path, '\x00') || strings.Contains(path, ":") || strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("skill resource path must be relative to the module root")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", fmt.Errorf("skill resource path escapes the module root")
		}
	}
	cleaned := filepath.Clean(filepath.FromSlash(path))
	if cleaned == "." || filepath.IsAbs(cleaned) || filepath.VolumeName(cleaned) != "" {
		return "", fmt.Errorf("skill resource path must be relative to the module root")
	}
	return cleaned, nil
}

func pathIsWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func parseSkillAliases(prefix string) []string {
	lines := strings.Split(strings.ReplaceAll(prefix, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil
	}
	var aliases []string
	seen := make(map[string]struct{})
	collect := func(value string) {
		for _, alias := range splitSkillAliasList(value) {
			alias = parseSkillAliasScalar(alias)
			if !validSkillAlias(alias) {
				continue
			}
			key := skillNameFoldKey(alias)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			aliases = append(aliases, alias)
		}
	}
	inAliases := false
	closed := false
	for _, rawLine := range lines[1:] {
		if strings.TrimSpace(rawLine) == "---" {
			closed = true
			break
		}
		trimmed := strings.TrimSpace(stripSkillYAMLComment(strings.TrimSpace(rawLine)))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(rawLine, " ") && !strings.HasPrefix(rawLine, "\t") {
			inAliases = false
			if key, value, ok := strings.Cut(trimmed, ":"); ok && strings.EqualFold(strings.TrimSpace(key), "aliases") {
				inAliases = true
				value = strings.TrimSpace(value)
				if value != "" {
					collect(strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(value, "]"), "[")))
					inAliases = false
				}
			}
			continue
		}
		if inAliases {
			if strings.HasPrefix(trimmed, "- ") {
				collect(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			} else {
				inAliases = false
			}
		}
	}
	if !closed {
		return nil
	}
	return aliases
}

func stripSkillYAMLComment(value string) string {
	var quote rune
	escaped := false
	for index, char := range value {
		if quote == '"' && char == '\\' && !escaped {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == '#' && (index == 0 || value[index-1] == ' ' || value[index-1] == '\t') {
			return strings.TrimSpace(value[:index])
		}
	}
	return value
}

func splitSkillAliasList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var items []string
	var item strings.Builder
	var quote rune
	for _, char := range value {
		if quote != 0 {
			item.WriteRune(char)
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
			item.WriteRune(char)
		case ',':
			items = append(items, item.String())
			item.Reset()
		default:
			item.WriteRune(char)
		}
	}
	items = append(items, item.String())
	return items
}

func parseSkillAliasScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if decoded, err := strconv.Unquote(value); err == nil {
			return strings.TrimSpace(decoded)
		}
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
}

func validSkillAlias(alias string) bool {
	if alias == "" || len([]rune(alias)) > 64 || strings.ContainsAny(alias, "/\\\t\r\n ") {
		return false
	}
	for _, char := range alias {
		if !(char == '-' || char == '_' || char == '.' || (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}
