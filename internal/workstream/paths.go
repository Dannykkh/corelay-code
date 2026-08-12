package workstream

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	stateDirName       = ".corelay"
	legacyStateDirName = ".aniclew"
	proxyStateDirName  = ".claude-proxy"
	workstreamsDir     = "workstreams"
	workstreamJSONName = "workstream.json"
	timelineName       = "timeline.jsonl"
	handoffsDir        = "handoffs"
)

func Root(workspace string) string {
	current := filepath.Join(workspace, stateDirName, workstreamsDir)
	if pathExists(current) {
		return current
	}
	for _, name := range []string{legacyStateDirName, proxyStateDirName} {
		legacy := filepath.Join(workspace, name, workstreamsDir)
		if pathExists(legacy) {
			return legacy
		}
	}
	return current
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

func Dir(workspace, id string) string {
	return filepath.Join(Root(workspace), sanitizeID(id))
}

func StatePath(workspace, id string) string {
	return filepath.Join(Dir(workspace, id), workstreamJSONName)
}

func TimelinePath(workspace, id string) string {
	return filepath.Join(Dir(workspace, id), timelineName)
}

func HandoffsDir(workspace, id string) string {
	return filepath.Join(Dir(workspace, id), handoffsDir)
}

func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "workstream"
	}
	return out
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "workstream"
	}
	if len(out) > 48 {
		return strings.TrimRight(out[:48], "-")
	}
	return out
}
