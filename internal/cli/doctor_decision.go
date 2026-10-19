package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/decision"
)

func checkDecision(add func(level, name, detail string), dir string, cfg *config.Config) {
	if cfg.Decision == nil || !cfg.Decision.Enabled {
		add("ok", "decision", "disabled")
	} else if err := cfg.Decision.Validate(); err != nil {
		add("fail", "decision", err.Error())
	} else {
		dc := decisionConfig(dir, cfg.Decision)
		if dc.KeyFile == "" {
			add("fail", "decision", "key file missing: "+dc.KeyFile)
		} else if _, err := os.Stat(dc.KeyFile); os.IsNotExist(err) {
			add("fail", "decision", "key file missing: "+dc.KeyFile)
		} else if _, err := config.ReadKeyFile("decision", dc.KeyFile); err != nil {
			add("fail", "decision", "key file unreadable: "+dc.KeyFile)
		} else {
			add("ok", "decision", fmt.Sprintf("%s via %s", dc.Model, dc.Backend))
		}
	}
	if info, err := os.Stat(filepath.Join(dir, decision.ShadowFile)); err == nil && info.Mode().Perm()&^0o600 != 0 {
		add("warn", "decision", "shadow log is world-readable")
	}
}
