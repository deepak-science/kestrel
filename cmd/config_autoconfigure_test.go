package cmd

import (
	"testing"

	"github.com/deepak-science/kestrel/internal/config"
)

func TestGlobalSections_KeepsGuards(t *testing.T) {
	off := false
	global := &config.Config{Guards: config.GuardsConfig{RequireCI: &off}}

	if got := globalSections(global); got.Guards.CI() {
		t.Error("require_ci: false was dropped from the rewritten global config")
	}
}
