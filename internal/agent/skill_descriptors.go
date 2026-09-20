package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	maxSkillMetadataPrefixBytes             = 8 * 1024
	defaultSkillBodyReadBytes               = 8 * 1024
	minSkillBodyReadBytes                   = 4 * 1024
	maxSkillBodyReadBytes                   = 8 * 1024
	maxSkillFileBytes                 int64 = 8 << 20
	maxSkillResourcesPerRun                 = 16
	maxSkillSnapshotBytesPerRun       int64 = 16 << 20
	maxSkillReadPagesPerRun                 = 5000
	maxSkillRelativePathBytes               = 1024
	maxSkillToolInputBytes                  = 8 * 1024
	maxRelevantSkillCandidates              = 3
	maxSkillCandidatePromptBytes            = 850
	maxSkillCandidateDescriptionRunes       = 80
	skillCandidatePromptHeaderBytes         = 300
)

// SkillDescriptor is bounded metadata for a skill body. The body is read only
// after an explicit slash selection or a model selection from the run's
// bounded natural-language candidate list.
type SkillDescriptor struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Source      string        `json:"source"`
	Path        string        `json:"path"`
	ModuleRoot  string        `json:"module_root"`
	Aliases     []string      `json:"aliases,omitempty"`
	Digest      string        `json:"digest"`
	Size        int64         `json:"size"`
	Namespace   string        `json:"namespace"`
	Shadowed    []SkillOrigin `json:"shadowed,omitempty"`
}

type skillRoot struct {
	path      string
	source    string
	namespace string
}

// SkillDescriptorRoots returns metadata only, retaining the configured root
// order and first-wins collision contract. File bodies are hashed as a stream
// but are not retained in the catalog.
func SkillDescriptorRoots(workDir, source string, projectDirs, extraDirs []string) []SkillDescriptor {
	roots := configuredSkillRoots(workDir, source, projectDirs, extraDirs)
	var descriptors []SkillDescriptor
	seen := make(map[string]int)
	for _, root := range roots {
		for _, descriptor := range skillDescriptorsFromDir(root) {
			key := skillNameFoldKey(descriptor.Name)
			if index, ok := seen[key]; ok {
				descriptors[index].Shadowed = append(descriptors[index].Shadowed, SkillOrigin{
					ID: descriptor.ID, Name: descriptor.Name, Description: descriptor.Description,
					Source: descriptor.Source, Namespace: descriptor.Namespace,
					Path: descriptor.Path, ModuleRoot: descriptor.ModuleRoot,
					Aliases: append([]string(nil), descriptor.Aliases...), Digest: descriptor.Digest,
					Size: descriptor.Size,
				})
				continue
			}
			seen[key] = len(descriptors)
			descriptors = append(descriptors, descriptor)
		}
	}
	return descriptors
}

func descriptorFromOrigin(origin SkillOrigin) SkillDescriptor {
	return SkillDescriptor{
		ID: origin.ID, Name: origin.Name, Description: origin.Description,
		Source: origin.Source, Path: origin.Path, Digest: origin.Digest,
		ModuleRoot: origin.ModuleRoot, Aliases: append([]string(nil), origin.Aliases...),
		Size:      origin.Size,
		Namespace: origin.Namespace,
	}
}

// SkillIndexExclusionPaths returns every configured skill root independent of
// the active source policy. RAG must not rediscover a disabled skill or its
// reference files as ordinary project documentation.
func SkillIndexExclusionPaths(workDir string, projectDirs, extraDirs []string) []string {
	roots := configuredSkillRoots(workDir, "all", projectDirs, extraDirs)
	paths := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root.path)
		if err != nil {
			continue
		}
		key := ragPathKey(absolute)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		paths = append(paths, absolute)
	}
	return paths
}

func configuredSkillRoots(workDir, source string, projectDirs, extraDirs []string) []skillRoot {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = "all"
	}
	if source == "none" {
		return nil
	}
	switch source {
	case "claude", "codex", "gemini", "all":
	default:
		return nil
	}
	var roots []skillRoot
	addRoot := func(path, source, namespace string) {
		if strings.TrimSpace(path) != "" {
			roots = append(roots, skillRoot{path: path, source: source, namespace: namespace})
		}
	}

	addRoot(filepath.Join(workDir, ".claude", "skills"), "claude", "project-claude")
	addRoot(filepath.Join(workDir, ".codex", "skills"), "codex", "project-codex")
	addRoot(filepath.Join(workDir, ".agents", "skills"), "agents", "project-agents")
	for index, dir := range projectDirs {
		addRoot(dir, "custom", fmt.Sprintf("project-custom-%d", index+1))
	}

	home, _ := os.UserHomeDir()
	addHomeRoot := func(vendor string) {
		if home != "" {
			addRoot(filepath.Join(home, "."+vendor, "skills"), vendor, "user-"+vendor)
		}
	}
	switch source {
	case "claude":
		addHomeRoot("claude")
	case "codex":
		addHomeRoot("codex")
	case "gemini":
		addHomeRoot("gemini")
	case "all":
		addHomeRoot("claude")
		addHomeRoot("codex")
		addHomeRoot("gemini")
	}
	for index, dir := range extraDirs {
		addRoot(dir, "custom", fmt.Sprintf("custom-%d", index+1))
	}
	return roots
}

func skillDescriptorsFromDir(root skillRoot) []SkillDescriptor {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		return nil
	}
	var descriptors []SkillDescriptor
	for _, entry := range entries {
		var name, path string
		if entry.IsDir() {
			name = entry.Name()
			path = filepath.Join(root.path, name, "SKILL.md")
		} else if strings.HasSuffix(entry.Name(), ".md") {
			name = strings.TrimSuffix(entry.Name(), ".md")
			path = filepath.Join(root.path, entry.Name())
		} else {
			continue
		}
		descriptor, ok := describeSkillFile(path, name, root.source, root.namespace)
		if ok {
			descriptors = append(descriptors, descriptor)
		}
	}
	return descriptors
}

func describeSkillFile(path, name, source, namespace string) (SkillDescriptor, bool) {
	file, err := os.Open(path)
	if err != nil {
		return SkillDescriptor{}, false
	}
	defer file.Close()

	prefix := make([]byte, maxSkillMetadataPrefixBytes)
	n, readErr := io.ReadFull(file, prefix)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return SkillDescriptor{}, false
	}
	if n == 0 {
		return SkillDescriptor{}, false
	}
	initialInfo, err := file.Stat()
	if err != nil || !initialInfo.Mode().IsRegular() || initialInfo.Size() <= 0 || initialInfo.Size() > maxSkillFileBytes {
		return SkillDescriptor{}, false
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return SkillDescriptor{}, false
	}
	resolvedPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return SkillDescriptor{}, false
	}
	moduleRoot, err := filepath.EvalSymlinks(filepath.Dir(absolutePath))
	if err != nil || !pathIsWithin(moduleRoot, resolvedPath) {
		return SkillDescriptor{}, false
	}
	hash := sha256.New()
	if _, err := hash.Write(prefix[:n]); err != nil {
		return SkillDescriptor{}, false
	}
	remaining := maxSkillFileBytes + 1 - int64(n)
	copied, err := io.Copy(hash, io.LimitReader(file, remaining))
	if err != nil || int64(n)+copied > maxSkillFileBytes || int64(n)+copied != initialInfo.Size() {
		return SkillDescriptor{}, false
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	identity := sha256.Sum256([]byte(namespace + "\x00" + name + "\x00" + path + "\x00" + digest))
	return SkillDescriptor{
		ID:          hex.EncodeToString(identity[:12]),
		Name:        name,
		Description: SkillDescription(string(prefix[:n])),
		Source:      source,
		Path:        resolvedPath,
		ModuleRoot:  moduleRoot,
		Aliases:     parseSkillAliases(string(prefix[:n])),
		Digest:      digest,
		Size:        int64(n) + copied,
		Namespace:   namespace,
	}, true
}

// LoadSkillBody is the bounded compatibility helper for eager legacy callers.
// Production slash and natural-language paths use SkillBodyReader pages.
func LoadSkillBody(descriptor SkillDescriptor) (string, error) {
	if strings.TrimSpace(descriptor.Path) == "" || !strings.HasPrefix(descriptor.Digest, "sha256:") {
		return "", fmt.Errorf("skill descriptor is invalid")
	}
	file, err := os.Open(descriptor.Path)
	if err != nil {
		return "", fmt.Errorf("skill body is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSkillFileBytes {
		return "", fmt.Errorf("skill body exceeds the supported size")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
	if err != nil || int64(len(content)) > maxSkillFileBytes {
		return "", fmt.Errorf("skill body exceeds the supported size")
	}
	digest := sha256.Sum256(content)
	if descriptor.Digest != "sha256:"+hex.EncodeToString(digest[:]) {
		return "", fmt.Errorf("skill body changed after discovery; refresh the skill list")
	}
	return string(content), nil
}

// relevantSkillCandidates ranks only descriptor metadata and returns a small,
// stable shortlist for the current request. Bodies are never consulted.
func relevantSkillCandidates(query string, skills []SkillDescriptor) []SkillDescriptor {
	queryTerms := skillMatchTerms(query)
	if len(queryTerms) == 0 || len(skills) == 0 {
		return nil
	}
	type scored struct {
		descriptor SkillDescriptor
		score      int
	}
	var ranked []scored
	for _, skill := range skills {
		candidates := []SkillDescriptor{skill}
		for _, shadow := range skill.Shadowed {
			candidates = append(candidates, SkillDescriptor{
				ID: shadow.ID, Name: shadow.Name, Description: shadow.Description,
				Source: shadow.Source, Path: shadow.Path, ModuleRoot: shadow.ModuleRoot,
				Aliases: append([]string(nil), shadow.Aliases...), Digest: shadow.Digest,
				Size:      shadow.Size,
				Namespace: shadow.Namespace,
			})
		}
		for _, candidate := range candidates {
			score := skillMatchScore(queryTerms, candidate)
			if score > 0 {
				ranked = append(ranked, scored{descriptor: candidate, score: score})
			}
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	var result []SkillDescriptor
	usedBytes := skillCandidatePromptHeaderBytes
	seen := make(map[string]struct{})
	for _, item := range ranked {
		id := item.descriptor.ID
		if _, ok := seen[id]; ok {
			continue
		}
		item.descriptor.Description = boundedSkillCandidateDescription(item.descriptor.Description)
		command := skillCommandName(item.descriptor, skills)
		cost := len(command) + len(item.descriptor.Description) + 40
		if len(result) >= maxRelevantSkillCandidates || usedBytes+cost > maxSkillCandidatePromptBytes {
			continue
		}
		seen[id] = struct{}{}
		usedBytes += cost
		result = append(result, item.descriptor)
	}
	return result
}

func boundedSkillCandidateDescription(description string) string {
	runes := []rune(description)
	if len(runes) > maxSkillCandidateDescriptionRunes {
		return strings.TrimSpace(string(runes[:maxSkillCandidateDescriptionRunes])) + "…"
	}
	return description
}

func skillMatchTerms(text string) map[string]struct{} {
	terms := make(map[string]struct{})
	var token strings.Builder
	flush := func() {
		value := strings.TrimSpace(token.String())
		token.Reset()
		if value == "" {
			return
		}
		folded := normalizeSkillMatchToken(value)
		if len([]rune(folded)) < 3 || skillMatchStopWords[folded] {
			return
		}
		terms[folded] = struct{}{}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			token.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func normalizeSkillMatchToken(value string) string {
	value = skillNameFoldKey(value)
	// Strip common Korean particles and connective endings so a query like
	// "크롤링" also matches a descriptor phrase like "크롤링하는 방법".
	for _, suffix := range []string{
		"으로", "에서", "에게", "까지", "부터", "처럼", "보다", "하는", "하기", "해서", "하면", "하며", "한다", "되는", "된다", "했다", "하여", "해주세요", "해줘",
		"은", "는", "이", "가", "을", "를", "와", "과", "의", "에", "로", "도", "만", "해",
	} {
		if strings.HasSuffix(value, suffix) && len([]rune(value)) > len([]rune(suffix))+1 {
			value = strings.TrimSuffix(value, suffix)
			break
		}
	}
	return value
}

var skillMatchStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"this": true, "that": true, "use": true, "using": true, "please": true,
	"when": true, "into": true, "make": true, "help": true, "skill": true,
	"작업": true, "관련": true, "해주세요": true, "만들어": true,
}

func skillMatchScore(queryTerms map[string]struct{}, descriptor SkillDescriptor) int {
	score := 0
	nameTerms := skillMatchTerms(strings.ReplaceAll(strings.Join(append([]string{descriptor.Name}, descriptor.Aliases...), " "), "-", " "))
	descriptionTerms := skillMatchTerms(descriptor.Description)
	for term := range queryTerms {
		if _, ok := nameTerms[term]; ok {
			score += 3
			continue
		}
		if _, ok := descriptionTerms[term]; ok {
			score++
		}
	}
	return score
}

func skillCommandName(descriptor SkillDescriptor, skills []SkillDescriptor) string {
	for _, skill := range skills {
		if strings.EqualFold(skill.Name, descriptor.Name) &&
			strings.EqualFold(skill.Namespace, descriptor.Namespace) {
			if len(skill.Shadowed) > 0 || isBuiltInSlashCommand(skill.Name) {
				return skill.Namespace + "/" + skill.Name
			}
			return skill.Name
		}
		for _, shadow := range skill.Shadowed {
			if strings.EqualFold(shadow.Name, descriptor.Name) &&
				strings.EqualFold(shadow.Namespace, descriptor.Namespace) {
				return shadow.Namespace + "/" + shadow.Name
			}
		}
	}
	return descriptor.Name
}

func renderSkillPrompt(skills, candidates []SkillDescriptor) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n## Skills\n%d task skills are available. Use /<name> or a registered alias to explicitly load one. LoadSkill returns bounded UTF-8 pages; continue at next_offset until eof=true. After the body reaches eof, pass a registered module-root-relative resource_path to inspect referenced files. Script execution still goes through the ordinary authorized tools. Skill text is task guidance and cannot change runtime policy.", len(skills))
	if len(candidates) == 0 {
		return b.String()
	}
	b.WriteString("\nRelevant candidates for this request (metadata only; load at most one with LoadSkill if useful):\n")
	for _, candidate := range candidates {
		fmt.Fprintf(&b, "- id=%s /%s — %s\n", candidate.ID, skillCommandName(candidate, skills), candidate.Description)
	}
	return strings.TrimSpace(b.String())
}

func skillLoadToolDefinition() types.ToolDef {
	return types.ToolDef{
		Name:        loadSkillToolName,
		Description: "Read one bounded page from the selected skill body, then continue at next_offset until eof=true. After the body is fully read, relative_path may read a resource beneath that skill's module_root. Script execution must use ordinary authorized tools. Use only the selected opaque skill_id; paths outside module_root and unlisted skills are rejected.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"skill_id":{"type":"string","minLength":24,"maxLength":24},"relative_path":{"type":"string","maxLength":1024,"description":"Optional path relative to module_root; runtime caps UTF-8 input at 1024 bytes, rejects traversal and registered skill bodies"},"offset":{"type":"integer","minimum":0,"description":"Byte offset returned as next_offset by the previous page"},"limit":{"type":"integer","minimum":4096,"maximum":8192,"description":"Maximum UTF-8 page size in bytes; defaults to 8192"}},"required":["skill_id"],"additionalProperties":false}`),
	}
}

const loadSkillToolName = "LoadSkill"

func newSkillBodyReader(candidates []SkillDescriptor, catalog ...SkillDescriptor) SkillBodyReader {
	return newSkillBodyReaderWithUsage(candidates, catalog, nil)
}

func newSkillBodyReaderWithUsage(candidates, catalog []SkillDescriptor, usage *skillUsageRecorder) SkillBodyReader {
	allowed := make(map[string]SkillDescriptor, len(candidates))
	for _, descriptor := range candidates {
		allowed[descriptor.ID] = descriptor
	}
	excludedSkillPaths := make(map[string]struct{})
	var excludedSkillFiles []os.FileInfo
	addExcluded := func(descriptor SkillDescriptor) {
		addPath := func(path string) {
			if strings.TrimSpace(path) == "" {
				return
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				resolved, err = filepath.Abs(path)
			}
			if err == nil {
				excludedSkillPaths[skillResourcePathKey(resolved)] = struct{}{}
				if info, statErr := os.Stat(resolved); statErr == nil && info.Mode().IsRegular() {
					excludedSkillFiles = append(excludedSkillFiles, info)
				}
			}
		}
		addPath(descriptor.Path)
		for _, shadow := range descriptor.Shadowed {
			addPath(shadow.Path)
		}
	}
	for _, descriptor := range candidates {
		addExcluded(descriptor)
	}
	for _, descriptor := range catalog {
		addExcluded(descriptor)
	}
	var mu sync.Mutex
	selectedID := ""
	readStates := make(map[string]skillBodyReadState)
	snapshots := make(map[string]skillFileSnapshot)
	attemptedResources := make(map[string]struct{})
	snapshotLoadErrors := make(map[string]string)
	var snapshotBytes int64
	readAttemptCount := 0
	return func(request SkillBodyReadRequest) (SkillBodyChunk, error) {
		descriptor, ok := allowed[request.SkillID]
		if !ok {
			return SkillBodyChunk{}, fmt.Errorf("skill candidate is unavailable")
		}
		mu.Lock()
		defer mu.Unlock()
		if selectedID != "" && selectedID != descriptor.ID {
			return SkillBodyChunk{}, fmt.Errorf("one skill has already been selected for this run")
		}
		pathKey := strings.TrimSpace(request.RelativePath)
		if pathKey != "" {
			cleanPath, err := cleanSkillRelativePath(pathKey)
			if err != nil {
				return SkillBodyChunk{}, err
			}
			pathKey = filepath.ToSlash(cleanPath)
		}
		state, exists := readStates[pathKey]
		if !exists {
			if request.Offset != 0 {
				return SkillBodyChunk{}, fmt.Errorf("skill resource continuation must start at offset 0")
			}
		} else if request.Offset != state.NextOffset || state.EOF {
			return SkillBodyChunk{}, fmt.Errorf("skill resource continuation offset is invalid")
		}
		if pathKey != "" {
			bodyState, bodyLoaded := readStates[""]
			if !bodyLoaded || !bodyState.EOF {
				return SkillBodyChunk{}, fmt.Errorf("read the complete skill instructions before opening a relative resource")
			}
			if !exists {
				if _, attempted := attemptedResources[pathKey]; !attempted {
					if len(attemptedResources) >= maxSkillResourcesPerRun {
						return SkillBodyChunk{}, fmt.Errorf("skill resource read limit reached for this run")
					}
					attemptedResources[pathKey] = struct{}{}
				}
			}
		}
		if message, failed := snapshotLoadErrors[pathKey]; failed {
			return SkillBodyChunk{}, fmt.Errorf("%s", message)
		}
		limit := request.Limit
		if limit == 0 {
			limit = defaultSkillBodyReadBytes
		}
		if limit < minSkillBodyReadBytes || limit > maxSkillBodyReadBytes {
			return SkillBodyChunk{}, fmt.Errorf("skill read limit must be between %d and %d bytes", minSkillBodyReadBytes, maxSkillBodyReadBytes)
		}
		if state.Pages >= maxSkillReadPagesPerRun || readAttemptCount >= maxSkillReadPagesPerRun {
			return SkillBodyChunk{}, fmt.Errorf("skill page read limit reached for this run")
		}
		readAttemptCount++
		snapshot, snapshotLoaded := snapshots[pathKey]
		if !snapshotLoaded {
			loadedSnapshot, loadErr := loadSkillFileSnapshot(descriptor, pathKey, maxSkillSnapshotBytesPerRun-snapshotBytes, excludedSkillPaths, excludedSkillFiles)
			if loadErr != nil {
				snapshotLoadErrors[pathKey] = loadErr.Error()
				return SkillBodyChunk{}, loadErr
			}
			snapshot = loadedSnapshot
			if pathKey == "" && descriptor.Digest != snapshot.Digest {
				message := "skill body changed after discovery; refresh the skill list"
				snapshotLoadErrors[pathKey] = message
				return SkillBodyChunk{}, fmt.Errorf("%s", message)
			}
			if snapshotBytes+int64(len(snapshot.Content)) > maxSkillSnapshotBytesPerRun {
				message := "skill snapshot byte limit reached for this run"
				snapshotLoadErrors[pathKey] = message
				return SkillBodyChunk{}, fmt.Errorf("%s", message)
			}
			snapshotBytes += int64(len(snapshot.Content))
			snapshots[pathKey] = snapshot
		} else if state.Digest != "" && state.Digest != snapshot.Digest {
			return SkillBodyChunk{}, fmt.Errorf("skill resource changed during continuation")
		}
		chunk, err := skillSnapshotChunk(descriptor, pathKey, snapshot, request.Offset, limit)
		if err != nil {
			return SkillBodyChunk{}, err
		}
		selectedID = descriptor.ID
		readStates[pathKey] = skillBodyReadState{
			NextOffset: chunk.NextOffset, TotalBytes: chunk.TotalBytes,
			Digest: chunk.Digest, Pages: state.Pages + 1, EOF: chunk.EOF,
		}
		if pathKey == "" && chunk.EOF && usage != nil {
			usage.record(descriptor, chunk.Digest)
		}
		return chunk, nil
	}
}

type skillUsageRecorder struct {
	mu     sync.Mutex
	skills []ReceiptSkill
}

func (r *skillUsageRecorder) record(descriptor SkillDescriptor, digest string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, skill := range r.skills {
		if skill.ID == descriptor.ID {
			return
		}
	}
	r.skills = append(r.skills, ReceiptSkill{
		ID: descriptor.ID, Name: descriptor.Name, Source: descriptor.Source,
		Namespace: descriptor.Namespace, Digest: digest,
	})
}

func (r *skillUsageRecorder) snapshot() []ReceiptSkill {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReceiptSkill(nil), r.skills...)
}

func skillResourcePathKey(path string) string {
	cleaned := filepath.Clean(path)
	if os.PathSeparator == '\\' {
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

func executeLoadSkill(input json.RawMessage, reader func(SkillBodyReadRequest) (SkillBodyChunk, error)) (string, bool) {
	if reader == nil {
		return "Relevant skill candidates are unavailable for this run", true
	}
	if len(input) > maxSkillToolInputBytes {
		return "Skill candidate request exceeds the supported input size", true
	}
	var args struct {
		SkillID      string `json:"skill_id"`
		RelativePath string `json:"relative_path,omitempty"`
		Offset       int64  `json:"offset,omitempty"`
		Limit        int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.SkillID) == "" {
		return "Invalid skill candidate request", true
	}
	if len(args.RelativePath) > maxSkillRelativePathBytes {
		return "Skill resource path exceeds the supported input size", true
	}
	chunk, err := reader(SkillBodyReadRequest{
		SkillID: args.SkillID, RelativePath: args.RelativePath,
		Offset: args.Offset, Limit: args.Limit,
	})
	if err != nil {
		return err.Error(), true
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return "Skill content could not be encoded", true
	}
	return string(encoded), false
}
