package server

import (
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
)

type resolvedSkillSettings struct {
	source      string
	projectDirs []string
	dirs        []string
}

// resolveSkillSettings captures the persisted project/global skill policy once
// at the API boundary. Durable sessions pass their stored workspace here.
func resolveSkillSettings(workDir string) (resolvedSkillSettings, error) {
	cfg, _, err := config.LoadChecked()
	if err != nil {
		return resolvedSkillSettings{}, err
	}
	settings, err := config.ResolveSkillSettings(cfg, workDir)
	if err != nil {
		return resolvedSkillSettings{}, err
	}
	return resolvedSkillSettings{
		source: settings.Source, projectDirs: settings.ProjectDirs, dirs: settings.Dirs,
	}, nil
}

func (settings resolvedSkillSettings) load(workDir string) []agent.SkillDescriptor {
	return agent.SkillDescriptorRoots(workDir, settings.source, settings.projectDirs, settings.dirs)
}

func loadConfiguredSkills(workDir string) ([]agent.SkillDescriptor, error) {
	settings, err := resolveSkillSettings(workDir)
	if err != nil {
		return nil, err
	}
	return settings.load(workDir), nil
}
