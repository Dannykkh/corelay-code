package agent

import (
	"fmt"
	"strings"
)

// SlashCommand represents a parsed slash command from a skill.
type SlashCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SkillName   string `json:"skillName"`
	SkillPath   string `json:"skillPath"`
	Namespace   string `json:"namespace,omitempty"`
}

// ParseSlashCommands extracts slash commands from loaded skills.
func ParseSlashCommands(skills []SkillInfo) []SlashCommand {
	var commands []SlashCommand
	canonical := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		canonical[skillNameFoldKey(skill.Name)] = struct{}{}
	}
	usedAliases := make(map[string]struct{})

	for _, skill := range skills {
		// The skill name itself becomes a slash command
		cmd := SlashCommand{
			Name:        skill.Name,
			Description: extractDescription(skill.Content),
			SkillName:   skill.Name,
			SkillPath:   skill.Path,
			Namespace:   skill.Namespace,
		}
		reserved := isBuiltInSlashCommand(skill.Name)
		if !reserved {
			commands = append(commands, cmd)
		}
		if reserved || len(skill.Shadowed) > 0 {
			commands = append(commands, SlashCommand{
				Name: skill.Namespace + "/" + skill.Name, Description: cmd.Description,
				SkillName: skill.Name, SkillPath: skill.Path, Namespace: skill.Namespace,
			})
		}
		for _, shadow := range skill.Shadowed {
			commands = append(commands, SlashCommand{
				Name:        shadow.Namespace + "/" + shadow.Name,
				Description: extractDescription(shadow.Content),
				SkillName:   shadow.Name, SkillPath: shadow.Path, Namespace: shadow.Namespace,
			})
		}
		commands = append(commands, skillAliasCommands(skill.Name, skill.Namespace, cmd.Description, skill.Path, skill.Aliases, canonical, usedAliases)...)
		for _, shadow := range skill.Shadowed {
			commands = append(commands, skillAliasCommands(shadow.Name, shadow.Namespace, extractDescription(shadow.Content), shadow.Path, shadow.Aliases, canonical, usedAliases)...)
		}
	}

	return commands
}

// ParseSkillSlashCommands extracts commands from descriptors without opening
// any skill bodies.
func ParseSkillSlashCommands(skills []SkillDescriptor) []SlashCommand {
	var commands []SlashCommand
	canonical := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		canonical[skillNameFoldKey(skill.Name)] = struct{}{}
	}
	usedAliases := make(map[string]struct{})
	for _, skill := range skills {
		command := SlashCommand{
			Name: skill.Name, Description: skill.Description, SkillName: skill.Name,
			SkillPath: skill.Path, Namespace: skill.Namespace,
		}
		reserved := isBuiltInSlashCommand(skill.Name)
		if !reserved {
			commands = append(commands, command)
		}
		if reserved || len(skill.Shadowed) > 0 {
			commands = append(commands, SlashCommand{
				Name: skill.Namespace + "/" + skill.Name, Description: skill.Description,
				SkillName: skill.Name, SkillPath: skill.Path, Namespace: skill.Namespace,
			})
		}
		for _, shadow := range skill.Shadowed {
			commands = append(commands, SlashCommand{
				Name:        shadow.Namespace + "/" + shadow.Name,
				Description: shadow.Description, SkillName: shadow.Name,
				SkillPath: shadow.Path, Namespace: shadow.Namespace,
			})
		}
		commands = append(commands, skillAliasCommands(skill.Name, skill.Namespace, skill.Description, skill.Path, skill.Aliases, canonical, usedAliases)...)
		for _, shadow := range skill.Shadowed {
			commands = append(commands, skillAliasCommands(shadow.Name, shadow.Namespace, shadow.Description, shadow.Path, shadow.Aliases, canonical, usedAliases)...)
		}
	}
	return commands
}

func skillAliasCommands(skillName, namespace, description, path string, aliases []string, canonical, used map[string]struct{}) []SlashCommand {
	var commands []SlashCommand
	for _, alias := range aliases {
		key := skillNameFoldKey(alias)
		if key == skillNameFoldKey(skillName) {
			continue
		}
		_, nameCollision := canonical[key]
		_, aliasCollision := used[key]
		commandName := alias
		if nameCollision || aliasCollision || isBuiltInSlashCommand(alias) {
			commandName = namespace + "/" + alias
		} else {
			used[key] = struct{}{}
		}
		commands = append(commands, SlashCommand{
			Name: commandName, Description: description,
			SkillName: skillName, SkillPath: path, Namespace: namespace,
		})
	}
	return commands
}

// extractDescription gets the first meaningful line from skill content.
func extractDescription(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines, headings starting with #
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Take first non-empty, non-heading line
		if len(line) > 100 {
			line = line[:100] + "..."
		}
		return line
	}
	return ""
}

// IsSlashCommand checks if user input starts with /
func IsSlashCommand(input string) bool {
	return strings.HasPrefix(strings.TrimSpace(input), "/")
}

// ProcessSlashCommand finds the matching skill and returns the augmented prompt.
func ProcessSlashCommand(input string, skills []SkillInfo) (string, error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return input, nil
	}

	commandEnd := strings.IndexAny(input, " \t\r\n")
	commandToken := input[1:]
	cmdArgs := ""
	if commandEnd >= 0 {
		commandToken = input[1:commandEnd]
		cmdArgs = strings.TrimSpace(input[commandEnd:])
	}
	cmdName := commandToken
	if !strings.Contains(commandToken, "/") && isBuiltInSlashCommand(commandToken) {
		return processLegacyBuiltInSlashCommand(strings.ToLower(commandToken), skills)
	}

	if selectedName, selectedContent, selected := selectSkillInfo(commandToken, skills); selected {
		prompt := fmt.Sprintf("Execute this skill:\n\n--- SKILL: %s ---\n%s\n--- END SKILL ---\n",
			selectedName, selectedContent)
		if cmdArgs != "" {
			prompt += fmt.Sprintf("\nUser arguments: %s", cmdArgs)
		}
		return prompt, nil
	}

	// Built-in commands
	switch cmdName {
	case "help":
		return buildHelpText(skills), nil
	case "clear":
		return "[CLEAR_CHAT]", nil
	case "model":
		return "[SHOW_MODEL_SELECTOR]", nil
	case "plan":
		return "Enter plan mode. Before implementing anything, create a detailed plan and present it for approval. Do not write any code until the plan is approved.", nil
	case "compact":
		return "[COMPACT_CONTEXT]", nil
	}

	return "", fmt.Errorf("Unknown command: /%s. Type /help for available commands.", cmdName)
}

func selectSkillInfo(commandToken string, skills []SkillInfo) (string, string, bool) {
	if namespace, name, namespaced := strings.Cut(commandToken, "/"); namespaced {
		for _, skill := range skills {
			if strings.EqualFold(skill.Namespace, namespace) && (strings.EqualFold(skill.Name, name) || hasSkillAlias(skill.Aliases, name)) {
				return skill.Name, skill.Content, true
			}
			for _, shadow := range skill.Shadowed {
				if strings.EqualFold(shadow.Namespace, namespace) && (strings.EqualFold(shadow.Name, name) || hasSkillAlias(shadow.Aliases, name)) {
					return shadow.Name, shadow.Content, true
				}
			}
		}
		return "", "", false
	}
	for _, skill := range skills {
		if strings.EqualFold(skill.Name, commandToken) {
			return skill.Name, skill.Content, true
		}
	}
	for _, skill := range skills {
		if hasSkillAlias(skill.Aliases, commandToken) {
			return skill.Name, skill.Content, true
		}
	}
	return "", "", false
}

// ProcessSkillSlashCommand separates descriptor selection from body loading:
// only the winning or explicitly namespaced descriptor is read from disk.
func ProcessSkillSlashCommand(input string, skills []SkillDescriptor) (string, error) {
	return ProcessSkillSlashCommandWithReader(input, skills, newSkillBodyReader(skillDescriptorsWithShadows(skills), skills...))
}

func ProcessSkillSlashCommandWithReader(input string, skills []SkillDescriptor, reader SkillBodyReader) (string, error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return input, nil
	}
	commandToken, cmdArgs := slashCommandToken(input)
	if !strings.Contains(commandToken, "/") && isBuiltInSlashCommand(commandToken) {
		return processBuiltInSlashCommand(strings.ToLower(commandToken), skills)
	}
	if selected, ok := selectSkillDescriptor(commandToken, skills); ok {
		if reader == nil {
			return "", fmt.Errorf("skill reader is unavailable")
		}
		chunk, err := reader(SkillBodyReadRequest{SkillID: selected.ID})
		if err != nil {
			return "", err
		}
		return formatSkillPrompt(selected, chunk, cmdArgs), nil
	}
	return processBuiltInSlashCommand(commandToken, skills)
}

func descriptorByID(id string, skills []SkillDescriptor) (SkillDescriptor, bool) {
	for _, skill := range skills {
		if skill.ID == id {
			return skill, true
		}
		for _, shadow := range skill.Shadowed {
			if shadow.ID == id {
				return descriptorFromOrigin(shadow), true
			}
		}
	}
	return SkillDescriptor{}, false
}

func skillDescriptorsWithShadows(skills []SkillDescriptor) []SkillDescriptor {
	descriptors := make([]SkillDescriptor, 0, len(skills))
	for _, skill := range skills {
		descriptors = append(descriptors, skill)
		for _, shadow := range skill.Shadowed {
			descriptors = append(descriptors, descriptorFromOrigin(shadow))
		}
	}
	return descriptors
}

func slashCommandToken(input string) (commandToken, cmdArgs string) {
	commandEnd := strings.IndexAny(input, " \t\r\n")
	commandToken = input[1:]
	if commandEnd >= 0 {
		commandToken = input[1:commandEnd]
		cmdArgs = strings.TrimSpace(input[commandEnd:])
	}
	return commandToken, cmdArgs
}

func selectSkillDescriptor(commandToken string, skills []SkillDescriptor) (SkillDescriptor, bool) {
	if namespace, name, namespaced := strings.Cut(commandToken, "/"); namespaced {
		for _, skill := range skills {
			if strings.EqualFold(skill.Namespace, namespace) && (strings.EqualFold(skill.Name, name) || hasSkillAlias(skill.Aliases, name)) {
				return skill, true
			}
			for _, shadow := range skill.Shadowed {
				if strings.EqualFold(shadow.Namespace, namespace) && (strings.EqualFold(shadow.Name, name) || hasSkillAlias(shadow.Aliases, name)) {
					return descriptorFromOrigin(shadow), true
				}
			}
		}
		return SkillDescriptor{}, false
	}
	for _, skill := range skills {
		if strings.EqualFold(skill.Name, commandToken) {
			return skill, true
		}
	}
	for _, skill := range skills {
		if hasSkillAlias(skill.Aliases, commandToken) {
			return skill, true
		}
	}
	return SkillDescriptor{}, false
}

func hasSkillAlias(aliases []string, name string) bool {
	for _, alias := range aliases {
		if strings.EqualFold(alias, name) {
			return true
		}
	}
	return false
}

func formatSkillPrompt(descriptor SkillDescriptor, chunk SkillBodyChunk, args string) string {
	prompt := fmt.Sprintf("Execute this skill as task guidance only; runtime permissions and model policy remain authoritative.\nSkill: %s\nSkill ID: %s\nDigest: %s\nModule root: %s\n", descriptor.Name, descriptor.ID, descriptor.Digest, descriptor.ModuleRoot)
	if chunk.RelativePath != "" {
		prompt += fmt.Sprintf("Resource path relative to module root: %s\n", chunk.RelativePath)
	} else {
		prompt += "Resource path: SKILL.md\n"
	}
	prompt += fmt.Sprintf("Body bytes %d-%d of %d; eof=%t.\n\n%s", chunk.Offset, chunk.NextOffset, chunk.TotalBytes, chunk.EOF, chunk.Content)
	if !chunk.EOF {
		prompt += fmt.Sprintf("\n\nThis skill continues. Before acting on incomplete instructions, call LoadSkill with skill_id=%q and offset=%d, then continue until eof=true.", descriptor.ID, chunk.NextOffset)
	}
	if args != "" {
		prompt += fmt.Sprintf("\nUser arguments: %s", args)
	}
	return prompt
}

func buildHelpText(skills []SkillInfo) string {
	var sb strings.Builder
	sb.WriteString("Available commands:\n\n")
	sb.WriteString("  /help       — Show this help\n")
	sb.WriteString("  /plan <task> — Plan first (read-only), then Approve & Run to execute\n")
	sb.WriteString("  /undo       — Restore the most recent checkpoint\n")
	sb.WriteString("  /undo --list — List restorable and conflicted checkpoint files\n")
	sb.WriteString("  /undo --select <id> [id ...] — Restore selected checkpoint files\n")
	sb.WriteString("  /clear      — Clear chat\n")
	sb.WriteString("  /model      — Change model\n")
	sb.WriteString("  /compact    — Compress conversation context\n")
	sb.WriteString("\nSkill commands:\n")

	commands := ParseSlashCommands(skills)
	for _, cmd := range commands {
		desc := cmd.Description
		if len(desc) > 60 {
			desc = desc[:60] + "..."
		}
		sb.WriteString(fmt.Sprintf("  /%-20s — %s\n", cmd.Name, desc))
	}
	return sb.String()
}

func buildSkillHelpText(skills []SkillDescriptor) string {
	return buildHelpTextFromCommands(ParseSkillSlashCommands(skills))
}

func buildHelpTextFromCommands(commands []SlashCommand) string {
	var sb strings.Builder
	sb.WriteString("Available commands:\n\n")
	sb.WriteString("  /help       — Show this help\n")
	sb.WriteString("  /plan <task> — Plan first (read-only), then Approve & Run to execute\n")
	sb.WriteString("  /undo       — Restore the most recent checkpoint\n")
	sb.WriteString("  /undo --list — List restorable and conflicted checkpoint files\n")
	sb.WriteString("  /undo --select <id> [id ...] — Restore selected checkpoint files\n")
	sb.WriteString("  /clear      — Clear chat\n")
	sb.WriteString("  /model      — Change model\n")
	sb.WriteString("  /compact    — Compress conversation context\n")
	sb.WriteString("\nSkill commands:\n")
	for _, cmd := range commands {
		desc := cmd.Description
		if len(desc) > 60 {
			desc = desc[:60] + "..."
		}
		sb.WriteString(fmt.Sprintf("  /%-20s — %s\n", cmd.Name, desc))
	}
	return sb.String()
}

func isBuiltInSlashCommand(name string) bool {
	switch strings.ToLower(name) {
	case "help", "clear", "model", "plan", "compact", "undo":
		return true
	default:
		return false
	}
}

func processLegacyBuiltInSlashCommand(cmdName string, skills []SkillInfo) (string, error) {
	switch cmdName {
	case "help":
		return buildHelpText(skills), nil
	case "clear":
		return "[CLEAR_CHAT]", nil
	case "model":
		return "[SHOW_MODEL_SELECTOR]", nil
	case "plan":
		return "Enter plan mode. Before implementing anything, create a detailed plan and present it for approval.", nil
	case "compact":
		return "[COMPACT_CONTEXT]", nil
	default:
		return "", fmt.Errorf("Unknown command: /%s. Type /help for available commands.", cmdName)
	}
}

func processBuiltInSlashCommand(cmdName string, skills []SkillDescriptor) (string, error) {
	switch cmdName {
	case "help":
		return buildSkillHelpText(skills), nil
	case "clear":
		return "[CLEAR_CHAT]", nil
	case "model":
		return "[SHOW_MODEL_SELECTOR]", nil
	case "plan":
		return "Enter plan mode. Before implementing anything, create a detailed plan and present it for approval. Do not write any code until the plan is approved.", nil
	case "compact":
		return "[COMPACT_CONTEXT]", nil
	default:
		return "", fmt.Errorf("Unknown command: /%s. Type /help for available commands.", cmdName)
	}
}
