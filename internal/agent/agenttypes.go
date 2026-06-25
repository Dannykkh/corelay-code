package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// AgentType defines a specialized agent role.
type AgentType struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SystemPrompt string   `json:"systemPrompt"`
	Tools        []string `json:"tools"`    // allowed tools (empty = all)
	ReadOnly     bool     `json:"readOnly"` // if true, no write tools
	Model        string   `json:"model"`    // override model (empty = inherit)
}

// BuiltinAgentTypes returns the predefined agent types.
func BuiltinAgentTypes() map[string]AgentType {
	return map[string]AgentType{
		"explorer": {
			Name:        "explorer",
			Description: "Fast codebase exploration — finds files, reads code, searches patterns",
			SystemPrompt: `You are an Explorer agent. Your job is to quickly find information in the codebase.
Use Glob to find files, Grep to search content, Read to read files.
Do NOT modify any files. Report findings concisely.`,
			Tools:    []string{"Read", "Glob", "Grep", "Bash", "LS"},
			ReadOnly: true,
		},
		"researcher": {
			Name:        "researcher",
			Description: "Deep research — analyzes architecture, traces data flow, understands patterns",
			SystemPrompt: `You are a Researcher agent. Deeply analyze code architecture, trace data flows, and understand design patterns.
Read multiple files to understand relationships. Report with file paths and line numbers.
Do NOT modify any files.`,
			Tools:    []string{"Read", "Glob", "Grep", "Bash", "LS"},
			ReadOnly: true,
		},
		"planner": {
			Name:        "planner",
			Description: "Implementation planning — designs approach, identifies files to modify, creates plan",
			SystemPrompt: `You are a Planner agent. Design implementation plans.
1. Analyze the current codebase structure
2. Identify which files need to be created or modified
3. Define step-by-step implementation plan
4. Note potential risks and dependencies
Do NOT modify any files. Output a structured plan.`,
			Tools:    []string{"Read", "Glob", "Grep", "Bash", "LS"},
			ReadOnly: true,
		},
		"coder": {
			Name:        "coder",
			Description: "Implementation — writes and edits code, runs tests",
			SystemPrompt: `You are a Coder agent. Implement changes precisely.
- Read files before editing
- Make minimal, focused changes
- Run tests after changes when possible
- Follow existing code patterns`,
			Tools: []string{}, // all tools
		},
		"reviewer": {
			Name:        "reviewer",
			Description: "Code review — checks quality, security, and correctness",
			SystemPrompt: `You are a Code Reviewer agent. Review code for:
1. Correctness — does it do what it should?
2. Security — any vulnerabilities? (OWASP top 10)
3. Performance — any bottlenecks?
4. Style — follows project conventions?
5. Tests — adequate coverage?
Report issues with file paths and line numbers. Suggest fixes.
Do NOT modify any files.`,
			Tools:    []string{"Read", "Glob", "Grep", "Bash"},
			ReadOnly: true,
		},
		"tester": {
			Name:        "tester",
			Description: "Test execution — runs tests, analyzes failures, writes test cases",
			SystemPrompt: `You are a Tester agent. Run tests and analyze results.
- Execute test suites
- Analyze failures and identify root causes
- Write new test cases for untested code
- Report coverage gaps`,
			Tools: []string{}, // all tools
		},
	}
}

// GetAgentType returns an agent type by name, or nil if not found.
func GetAgentType(name string) *AgentType {
	types := BuiltinAgentTypes()
	if t, ok := types[name]; ok {
		return &t
	}
	return nil
}

// LoadCustomAgentTypes reads agent definitions from project .claude/agents/ directory.
func LoadCustomAgentTypes(workDir string) map[string]AgentType {
	types := map[string]AgentType{}
	if strings.TrimSpace(workDir) == "" {
		return types
	}

	dir := filepath.Join(workDir, ".claude", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return types
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		agentType, ok := readCustomAgentType(path)
		if !ok {
			continue
		}
		if strings.TrimSpace(agentType.Name) == "" {
			agentType.Name = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}
		types[agentType.Name] = agentType
	}
	return types
}

func readCustomAgentType(path string) (AgentType, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentType{}, false
	}
	fields, body, ok := parseAgentTypeMarkdown(string(data))
	if !ok {
		return AgentType{}, false
	}

	agentType := AgentType{
		Name:         cleanAgentScalar(fields["name"]),
		Description:  cleanAgentScalar(fields["description"]),
		SystemPrompt: strings.TrimSpace(body),
		Tools:        parseAgentTools(fields["tools"]),
		Model:        cleanAgentScalar(fields["model"]),
	}
	if prompt := strings.TrimSpace(fields["systemPrompt"]); prompt != "" {
		agentType.SystemPrompt = prompt
	}
	if prompt := strings.TrimSpace(fields["system_prompt"]); prompt != "" {
		agentType.SystemPrompt = prompt
	}
	agentType.ReadOnly = parseAgentBool(fields["readOnly"]) ||
		parseAgentBool(fields["read_only"]) ||
		parseAgentBool(fields["readonly"])
	return agentType, true
}

func parseAgentTypeMarkdown(content string) (map[string]string, string, bool) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, "", false
	}

	fields := map[string]string{}
	i := 1
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			return fields, strings.Join(lines[i+1:], "\n"), true
		}
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}
		key, raw, ok := strings.Cut(line, ":")
		if !ok {
			i++
			continue
		}
		key = strings.TrimSpace(key)
		value := strings.TrimSpace(raw)
		if key == "" {
			i++
			continue
		}

		if isAgentBlockScalar(value) {
			block, next := collectAgentIndentedBlock(lines, i+1)
			fields[key] = strings.Join(block, "\n")
			i = next
			continue
		}
		if value == "" {
			block, next := collectAgentIndentedBlock(lines, i+1)
			if len(block) > 0 {
				fields[key] = strings.Join(block, "\n")
				i = next
				continue
			}
		}
		fields[key] = value
		i++
	}
	return nil, "", false
}

func collectAgentIndentedBlock(lines []string, start int) ([]string, int) {
	var block []string
	i := start
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			break
		}
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		block = append(block, strings.TrimSpace(line))
		i++
	}
	return block, i
}

func isAgentBlockScalar(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	return value[0] == '|' || value[0] == '>'
}

func cleanAgentScalar(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "\"'")
	return strings.Join(strings.Fields(value), " ")
}

func parseAgentTools(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")

	var tools []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "-")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			part = strings.Trim(part, "\"'")
			if part != "" {
				tools = append(tools, part)
			}
		}
	}
	return tools
}

func parseAgentBool(value string) bool {
	switch strings.ToLower(cleanAgentScalar(value)) {
	case "true", "yes", "1", "on":
		return true
	default:
		return false
	}
}
