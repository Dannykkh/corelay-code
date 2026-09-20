package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/config"
)

func TestImproveCLIRejectsMissingConsentBaselineAndMeasurementOnly(t *testing.T) {
	dependencies := defaultCLIDependencies()
	dependencies.loadConfig = func() config.Config { return config.Config{} }
	for _, args := range [][]string{
		{"improve"},
		{"improve", "--confirm"},
		{"improve", "--confirm", "--baseline", "fixture", "--measurement-only"},
		{"run", "--confirm", "--baseline", "fixture"},
		{"run", "--confirm", "--learn"},
		{"improve", "--learn", "--confirm"},
	} {
		var stdout, stderr bytes.Buffer
		code := runCLI(context.Background(), args, &stdout, &stderr, dependencies)
		if code != 2 || stdout.Len() != 0 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}
