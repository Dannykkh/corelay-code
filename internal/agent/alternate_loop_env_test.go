package agent

import "testing"

// isolateAlternateLoopEnvironment keeps alternate-loop integration fixtures
// independent from the developer machine's global instructions, skills, and
// background memory hooks. Production runs intentionally inherit those inputs;
// focused tests must provide their own complete context instead.
func isolateAlternateLoopEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
}
