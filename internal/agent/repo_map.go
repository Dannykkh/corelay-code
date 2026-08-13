package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultRepoMapFiles      = 400
	maximumRepoMapFiles      = 2000
	maximumRepoMapCandidates = 20_000
	maximumRepoMapBytes      = 64 << 10
	maximumRepoMapFileBytes  = 512 << 10
	maximumRepoMapSymbols    = 16
	maximumRepoMapSignature  = 240
)

var repoMapDeclarationPatterns = map[string][]*regexp.Regexp{
	".ts": {
		regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:async\s+)?(?:function|class|interface|type|enum|namespace)\s+[A-Za-z_$][A-Za-z0-9_$]*`),
	},
	".js": {
		regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:async\s+)?(?:function|class)\s+[A-Za-z_$][A-Za-z0-9_$]*`),
	},
	".py": {
		regexp.MustCompile(`^(?:async\s+)?(?:def|class)\s+[A-Za-z_][A-Za-z0-9_]*`),
	},
	".rs": {
		regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:fn|struct|enum|trait|type|mod)\s+[A-Za-z_][A-Za-z0-9_]*`),
	},
}

var repoMapArrowDeclarationPattern = regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+[A-Za-z_$][A-Za-z0-9_$]*\s*=`)

var repoMapSourceExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rs": true, ".java": true, ".kt": true, ".kts": true,
	".cs": true, ".c": true, ".h": true, ".hpp": true, ".cpp": true,
	".cc": true, ".rb": true, ".php": true, ".swift": true, ".scala": true,
}

var repoMapSkippedDirectories = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".recovery": true,
	".corelay": true, ".aniclew": true, ".termsnap": true, ".claude": true,
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"coverage": true, "target": true, "bin": true, "obj": true,
}

type repoMapInput struct {
	Path              string `json:"path"`
	IncludeSignatures *bool  `json:"include_signatures"`
	MaxFiles          int    `json:"max_files"`
}

type repoMapEntry struct {
	path       string
	signatures []string
}

func executeRepoMap(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("RepoMap", input, workDir)
	if err != nil {
		return "RepoMap blocked: " + err.Error(), true
	}
	var args repoMapInput
	if err := json.Unmarshal(input, &args); err != nil {
		return "RepoMap blocked: invalid input", true
	}
	maximum := args.MaxFiles
	if maximum <= 0 {
		maximum = defaultRepoMapFiles
	}
	if maximum > maximumRepoMapFiles {
		maximum = maximumRepoMapFiles
	}
	includeSignatures := args.IncludeSignatures == nil || *args.IncludeSignatures
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	entries, total, scanTruncated, err := buildRepoMap(ctx, workDir, paths.one("path"), includeSignatures, maximum)
	if err != nil {
		if ctx.Err() != nil {
			return "RepoMap canceled before completion", true
		}
		return "RepoMap could not inspect the requested workspace path", true
	}
	return renderRepoMap(entries, total, scanTruncated), false
}

func buildRepoMap(ctx context.Context, workDir, root string, includeSignatures bool, maximum int) ([]repoMapEntry, int, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	if maximum <= 0 {
		maximum = defaultRepoMapFiles
	}
	if maximum > maximumRepoMapFiles {
		maximum = maximumRepoMapFiles
	}
	canonicalWorkDir, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, 0, false, err
	}
	canonicalRoot, err := canonicalizeTarget(root)
	if err != nil || !pathWithin(canonicalRoot, canonicalWorkDir) {
		return nil, 0, false, errors.New("repository map root is outside the workspace")
	}
	rootInfo, err := os.Stat(canonicalRoot)
	if err != nil || !rootInfo.IsDir() {
		return nil, 0, false, errors.New("repository map root is not a directory")
	}
	paths := make([]string, 0, maximum+1)
	scanTruncated := false
	err = filepath.WalkDir(canonicalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == canonicalRoot {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if repoMapSkippedDirectories[strings.ToLower(name)] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximumRepoMapFileBytes {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(name))
		if !repoMapSourceExtensions[extension] {
			return nil
		}
		paths = append(paths, path)
		if len(paths) >= maximumRepoMapCandidates {
			scanTruncated = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, 0, scanTruncated, err
	}
	sort.Slice(paths, func(i, j int) bool {
		left, _ := filepath.Rel(canonicalWorkDir, paths[i])
		right, _ := filepath.Rel(canonicalWorkDir, paths[j])
		return filepath.ToSlash(left) < filepath.ToSlash(right)
	})
	total := len(paths)
	if len(paths) > maximum {
		paths = paths[:maximum]
	}
	result := make([]repoMapEntry, 0, len(paths))
	for _, path := range paths {
		relative, err := filepath.Rel(canonicalWorkDir, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		item := repoMapEntry{path: boundedRepoMapPath(filepath.ToSlash(relative))}
		if includeSignatures {
			item.signatures = repoMapSignatures(path)
		}
		result = append(result, item)
	}
	return result, total, scanTruncated, nil
}

func repoMapSignatures(path string) []string {
	content, ok := readRepoMapSource(path)
	if !ok {
		return nil
	}
	if strings.EqualFold(filepath.Ext(path), ".go") {
		return goRepoMapSignatures(path, content)
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil
	}
	extension := strings.ToLower(filepath.Ext(path))
	if extension == ".tsx" {
		extension = ".ts"
	} else if extension == ".jsx" {
		extension = ".js"
	}
	patterns := repoMapDeclarationPatterns[extension]
	if len(patterns) == 0 {
		return nil
	}
	result := make([]string, 0, maximumRepoMapSymbols)
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		var declaration string
		for _, pattern := range patterns {
			if match := pattern.FindString(line); match != "" {
				declaration = match
				break
			}
		}
		if declaration == "" && (extension == ".ts" || extension == ".js") && strings.Contains(line, "=>") {
			if match := repoMapArrowDeclarationPattern.FindString(line); match != "" {
				declaration = strings.TrimSpace(strings.TrimSuffix(match, "=")) + " = (...) =>"
			}
		}
		if declaration == "" {
			continue
		}
		result = append(result, boundedRepoMapSignature(declaration))
		if len(result) == maximumRepoMapSymbols {
			break
		}
	}
	return result
}

func readRepoMapSource(path string) ([]byte, bool) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > maximumRepoMapFileBytes {
		return nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() > maximumRepoMapFileBytes {
		return nil, false
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumRepoMapFileBytes+1))
	if err != nil || len(content) > maximumRepoMapFileBytes {
		return nil, false
	}
	return content, true
}

func goRepoMapSignatures(path string, content []byte) []string {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, path, content, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	result := make([]string, 0, maximumRepoMapSymbols)
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.GenDecl:
			if typed.Tok != token.TYPE {
				continue
			}
			for _, spec := range typed.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				kind := "type"
				switch typeSpec.Type.(type) {
				case *ast.StructType:
					kind = "struct"
				case *ast.InterfaceType:
					kind = "interface"
				}
				result = append(result, "type "+typeSpec.Name.Name+" "+kind)
			}
		case *ast.FuncDecl:
			var signature bytes.Buffer
			signature.WriteString("func ")
			if typed.Recv != nil && len(typed.Recv.List) > 0 {
				signature.WriteByte('(')
				_ = format.Node(&signature, set, typed.Recv.List[0].Type)
				signature.WriteString(") ")
			}
			signature.WriteString(typed.Name.Name)
			var functionType bytes.Buffer
			_ = format.Node(&functionType, set, typed.Type)
			signature.WriteString(strings.TrimPrefix(functionType.String(), "func"))
			result = append(result, boundedRepoMapSignature(signature.String()))
		}
		if len(result) >= maximumRepoMapSymbols {
			return result[:maximumRepoMapSymbols]
		}
	}
	return result
}

func boundedRepoMapSignature(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = sanitizeRepoMapText(value)
	if len(value) <= maximumRepoMapSignature {
		return value
	}
	value = value[:maximumRepoMapSignature]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}

func boundedRepoMapPath(value string) string {
	const maximumPathBytes = 512
	value = sanitizeRepoMapText(value)
	if len(value) <= maximumPathBytes {
		return value
	}
	value = value[:maximumPathBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}

func sanitizeRepoMapText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

func renderRepoMap(entries []repoMapEntry, total int, scanTruncated bool) string {
	var output strings.Builder
	totalLabel := fmt.Sprintf("%d", total)
	if scanTruncated {
		totalLabel = "at least " + totalLabel
	}
	fmt.Fprintf(&output, "Repository map (%d of %s source files; relative paths; declaration signatures only)\n", len(entries), totalLabel)
	for _, entry := range entries {
		block := entry.path + ":\n"
		for _, signature := range entry.signatures {
			block += "  " + signature + "\n"
		}
		if output.Len()+len(block)+128 > maximumRepoMapBytes {
			output.WriteString("... output byte limit reached\n")
			return output.String()
		}
		output.WriteString(block)
	}
	if total > len(entries) {
		fmt.Fprintf(&output, "... %d additional source files omitted\n", total-len(entries))
	}
	if scanTruncated {
		output.WriteString("... candidate scan limit reached\n")
	}
	return output.String()
}
