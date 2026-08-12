package sandbox

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
)

// BuildEnvironment constructs a deterministic child environment from the
// explicit filter contract. It always returns a non-nil slice so os/exec does
// not interpret an empty environment as "inherit everything".
func BuildEnvironment(spec EnvironmentSpec) ([]string, error) {
	type entry struct {
		name  string
		value string
	}
	selected := make(map[string]entry, len(spec.Inherit)+len(spec.Set))

	for _, name := range spec.Inherit {
		if err := validateEnvironmentName(name); err != nil {
			return nil, err
		}
		identity := environmentNameIdentity(name)
		if _, exists := selected[identity]; exists {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			selected[identity] = entry{name: name, value: value}
		}
	}
	setNames := make([]string, 0, len(spec.Set))
	for name := range spec.Set {
		setNames = append(setNames, name)
	}
	sort.Strings(setNames)
	explicitNames := make(map[string]string, len(setNames))
	for _, name := range setNames {
		value := spec.Set[name]
		if err := validateEnvironmentName(name); err != nil {
			return nil, err
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("environment value for %q contains NUL", name)
		}
		identity := environmentNameIdentity(name)
		if previous, exists := explicitNames[identity]; exists && previous != name {
			return nil, fmt.Errorf("duplicate environment names %q and %q", previous, name)
		}
		explicitNames[identity] = name
		selected[identity] = entry{name: name, value: value}
	}

	identities := make([]string, 0, len(selected))
	for identity := range selected {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	environment := make([]string, 0, len(identities))
	for _, identity := range identities {
		item := selected[identity]
		environment = append(environment, item.name+"="+item.value)
	}
	return environment, nil
}

func validateEnvironmentName(name string) error {
	if name == "" {
		return fmt.Errorf("environment name is empty")
	}
	if strings.Contains(name, "=") || strings.IndexByte(name, 0) >= 0 {
		return fmt.Errorf("invalid environment name %q", name)
	}
	return nil
}

func environmentNameIdentity(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
