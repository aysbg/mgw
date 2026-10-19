package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/decision"
)

// ExitDecisionUnavailable means the decision layer is disabled or unconfigured.
const ExitDecisionUnavailable = 3

// Decide implements the standalone decision command. State is read only from a
// file; neither this command nor its shadow log participates in routing.
func Decide(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateFile := fs.String("state-file", "", "file containing the state (required)")
	backend := fs.String("backend", "", "override decision backend: ollama or jev")
	model := fs.String("model", "", "override decision model")
	asJSON := fs.Bool("json", false, "print the decision result as JSON")
	shadow := fs.Bool("shadow", false, "append a shadow record instead of printing the result")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if *stateFile == "" || fs.NArg() != 0 {
		return fail(stderr, "decide", config.Fail(config.CodeMissingValue, "--state-file is required; positional state is not accepted"))
	}
	if *backend != "" && *backend != decision.BackendOllama && *backend != decision.BackendJev {
		return fail(stderr, "decide", config.Fail(config.CodeBadValue, "decision backend must be ollama or jev"))
	}
	dir, err := deps.configDir()
	if err != nil {
		return decisionUnavailable(stderr, "decision_unconfigured", err.Error())
	}
	cfg, err := config.Load(dir)
	if err != nil {
		var failure *config.Failure
		if errors.As(err, &failure) && failure.Code == config.CodeMissingConfig {
			return decisionUnavailable(stderr, "decision_unconfigured", err.Error())
		}
		return fail(stderr, "decide", err)
	}
	if cfg.Decision == nil {
		return decisionUnavailable(stderr, "decision_unconfigured", "decision block is absent")
	}
	if !cfg.Decision.Enabled {
		return decisionUnavailable(stderr, "decision_disabled", "decision layer is disabled")
	}
	dc := decisionConfig(dir, cfg.Decision)
	if *backend != "" {
		dc.Backend = *backend
	}
	if *model != "" {
		dc.Model = *model
	}
	if dc.KeyFile == "" || dc.Model == "" || dc.BaseURL == "" {
		return decisionUnavailable(stderr, "decision_unconfigured", "decision model, base_url and key_file are required")
	}
	state, err := readFile(*stateFile)
	if err != nil {
		return fail(stderr, "decide", fmt.Errorf("decision_state_file: %w", err))
	}
	result, err := dc.Classify(context.Background(), string(state))
	if err != nil {
		return decisionUnavailable(stderr, "decision_unconfigured", err.Error())
	}
	if *shadow {
		if err := dc.AppendShadow(filepath.Join(dir, decision.ShadowFile), string(state), result, deps.Now()); err != nil {
			return fail(stderr, "decide", err)
		}
		_, err = fmt.Fprintln(stdout, "shadow: recorded")
	} else if *asJSON {
		err = json.NewEncoder(stdout).Encode(result)
	} else {
		_, err = fmt.Fprintf(stdout, "type: %s\nsize: %s\nrisk_flags: %v\nscout: %t\nconfidence: %.2f\nbackend: %s\ndecision_model: %s\nlatency_ms: %d\n",
			result.Type, result.Size, result.RiskFlags, result.Scout, result.Confidence, result.Backend, result.DecisionModel, result.LatencyMS)
		if err == nil && result.Fallback {
			_, err = fmt.Fprintf(stdout, "fallback_reason: %s\n", result.FallbackReason)
		}
	}
	if err != nil {
		return fail(stderr, "decide", err)
	}
	return ExitOK
}

func decisionUnavailable(stderr io.Writer, reason, detail string) int {
	_, _ = fmt.Fprintf(stderr, "mgw decide: %s: %s\n", reason, detail)
	return ExitDecisionUnavailable
}

func decisionConfig(dir string, block *config.Decision) decision.Config {
	key := block.KeyFile
	if key != "" && !filepath.IsAbs(key) {
		key = filepath.Join(dir, key)
	}
	return decision.Config{
		Backend: block.Backend, BaseURL: block.BaseURL, Model: block.Model, KeyFile: key,
		Timeout:         time.Duration(block.TimeoutMS) * time.Millisecond,
		ConfidenceFloor: block.ConfidenceFloor, Margin: block.Margin,
	}
}
