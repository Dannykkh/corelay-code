package workstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("workstream: marshal json: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("workstream: ensure dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("workstream: write temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("workstream: rename: %w", err)
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("workstream: read json: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("workstream: parse json: %w", err)
	}
	return nil
}

func appendJSONLine(path string, v any) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("workstream: ensure dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("workstream: open timeline: %w", err)
	}
	defer f.Close()
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("workstream: marshal event: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("workstream: append event: %w", err)
	}
	return nil
}

func readJSONLines[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return []T{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workstream: open timeline: %w", err)
	}
	defer f.Close()

	var out []T
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var item T
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, fmt.Errorf("workstream: parse timeline: %w", err)
		}
		out = append(out, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("workstream: scan timeline: %w", err)
	}
	return out, nil
}
