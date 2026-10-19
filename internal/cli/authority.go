package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/aysbg/mgw/internal/config"
	"github.com/aysbg/mgw/internal/policy"
	"github.com/aysbg/mgw/internal/provider"
	"github.com/aysbg/mgw/internal/proxy"
)

type authorityRoute struct {
	RunID           string   `json:"run_id"`
	Attempt         int      `json:"attempt"`
	BudgetMaxTokens int64    `json:"budget_max_tokens"`
	Primary         string   `json:"primary"`
	Chain           []string `json:"chain"`
	PolicySHA256    string   `json:"policy_sha256"`
	RegistrySHA256  string   `json:"registry_sha256"`
}

type authorityOutput struct {
	Out       string    `json:"out"`
	Runtime   string    `json:"runtime"`
	Authority string    `json:"authority"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Authority emits the runtime and secret authority for one route, without
// copying provider credentials. All input checks precede output creation.
func Authority(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("authority", flag.ContinueOnError)
	fs.SetOutput(stderr)
	routeFile := fs.String("route", "", "route JSON file (required)")
	out := fs.String("out", "", "private output directory (required)")
	deadline := fs.Int64("deadline-s", 900, "attempt deadline in seconds")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if *routeFile == "" || *out == "" || fs.NArg() != 0 || *deadline <= 0 || *deadline > int64((1<<63-1)/time.Second)-60 {
		return fail(stderr, "authority", config.Fail("route_malformed", "require --route, --out and a positive, bounded --deadline-s"))
	}
	result, err := emitAuthority(*routeFile, *out, *deadline, deps)
	if err != nil {
		return fail(stderr, "authority", err)
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return ExitError
	}
	return ExitOK
}

func readAuthorityRoute(path string) (authorityRoute, map[string][]string, []string, error) {
	var route authorityRoute
	data, err := readFile(path)
	if err != nil || json.Unmarshal(data, &route) != nil || strings.TrimSpace(route.RunID) == "" || route.Primary == "" || route.Attempt < 1 || route.BudgetMaxTokens <= 0 {
		return route, nil, nil, config.Fail("route_malformed", "invalid route document or required fields")
	}
	models := make(map[string][]string)
	var order []string
	for _, ref := range append([]string{route.Primary}, route.Chain...) {
		id, model, ok := strings.Cut(ref, "/")
		model, effort, hasEffort := strings.Cut(model, "#")
		if !ok || !provider.ValidID(id) || model == "" || len(model) > 256 || strings.IndexFunc(ref, unicode.IsSpace) >= 0 || strings.IndexFunc(ref, unicode.IsControl) >= 0 || (hasEffort && (effort == "" || strings.Contains(effort, "#"))) {
			return route, nil, nil, config.Fail("route_malformed", "invalid provider/model reference")
		}
		if _, exists := models[id]; !exists {
			order = append(order, id)
		}
		if !slices.Contains(models[id], model) {
			models[id] = append(models[id], model)
		}
	}
	return route, models, order, nil
}

func authorityProviders(dir string, models map[string][]string, order []string) ([]provider.Provider, error) {
	// The install schema does not expose allowed_models or credential_file.
	// Read this projection locally; only a non-empty allowlist narrows the
	// models named by the route from the pinned registry.
	var cfg struct {
		Providers []provider.Provider `json:"providers"`
	}
	data, err := readFile(config.ConfigPath(dir))
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return nil, config.Fail("provider_not_configured", "cannot read provider configuration")
	}
	var selected []provider.Provider
	for _, id := range order {
		var matches []provider.Provider
		for _, p := range cfg.Providers {
			if p.ID == id {
				matches = append(matches, p)
			}
		}
		if len(matches) != 1 {
			return nil, config.Fail("provider_not_configured", "route provider must have exactly one configuration")
		}
		p := matches[0]
		for _, model := range models[id] {
			if len(p.AllowedModels) > 0 && !slices.Contains(p.AllowedModels, model) {
				return nil, config.Fail("model_not_allowed", "route model is not in the configured allowlist")
			}
		}
		p.AllowedModels = models[id]
		if p.CredentialFile == "" {
			p.CredentialFile = config.CredentialPath(dir, id)
		} else if !filepath.IsAbs(p.CredentialFile) {
			p.CredentialFile = filepath.Join(dir, p.CredentialFile)
		}
		p.CredentialFile, err = filepath.Abs(p.CredentialFile)
		if err != nil {
			return nil, config.Fail("credential_missing", "cannot resolve credential file")
		}
		if _, err := p.Credential(); err != nil {
			return nil, config.Fail("credential_missing", "credential file is missing or unusable")
		}
		if p.Timeouts == (provider.Timeouts{}) {
			p.Timeouts = provider.Timeouts{ConnectMS: 10000, TTFBMS: 60000, IdleMS: 60000, OverallMS: 900000}
		}
		if p.IncludeStreamUsage == nil {
			// Match the install config's zero-value boolean, not the runtime's
			// dialect-specific default.
			p.IncludeStreamUsage = new(bool)
		}
		if err := p.Validate(); err != nil {
			return nil, config.Fail("provider_not_configured", "invalid runtime provider configuration")
		}
		selected = append(selected, p)
	}
	return selected, nil
}

func emitAuthority(routeFile, out string, deadline int64, deps Deps) (authorityOutput, error) {
	var result authorityOutput
	route, models, order, err := readAuthorityRoute(routeFile)
	if err != nil {
		return result, err
	}
	dir, err := deps.configDir()
	if err != nil {
		return result, config.Fail("provider_not_configured", "cannot find provider configuration")
	}
	providers, err := authorityProviders(dir, models, order)
	if err != nil {
		return result, err
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return result, config.Fail("out_not_private", "cannot resolve output directory")
	}
	if err := checkAuthorityOut(out); err != nil {
		return result, err
	}
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	expires := now().UTC().Add(time.Duration(deadline+60) * time.Second).Truncate(time.Second)
	result = authorityOutput{Out: out, Runtime: filepath.Join(out, "runtime.json"), Authority: filepath.Join(out, "authority.json"), ExpiresAt: expires}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return authorityOutput{}, errors.New("cannot generate authority token")
	}
	zeroHash := strings.Repeat("0", 64)
	if route.PolicySHA256 == "" {
		route.PolicySHA256 = zeroHash
	}
	if route.RegistrySHA256 == "" {
		route.RegistrySHA256 = zeroHash
	}
	auth := policy.Authority{
		Token: base64.RawURLEncoding.EncodeToString(secret), RunID: route.RunID, Attempt: route.Attempt,
		ExpiresAt: expires, Providers: models, Budget: policy.Budget{MaxTokens: route.BudgetMaxTokens},
		Concurrency: 2, PolicySHA256: route.PolicySHA256, RegistrySHA256: route.RegistrySHA256,
	}
	rt := proxy.Runtime{
		AuthorityFile: result.Authority, Providers: providers, LedgerDir: filepath.Join(out, "ledger"),
		AuditFile: filepath.Join(out, "audit.jsonl"), ReadyFile: filepath.Join(out, "ready.json"),
		CleanupTimeoutMS: 5000, DefaultReserveTokens: 2000, MaxReservationTokens: 8000,
	}
	authData, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return authorityOutput{}, errors.New("cannot encode authority")
	}
	runtimeData, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return authorityOutput{}, errors.New("cannot encode runtime")
	}
	if err := writeAuthorityOut(out, runtimeData, authData); err != nil {
		return authorityOutput{}, config.Fail("out_not_private", "cannot write private output directory")
	}
	return result, nil
}

func checkAuthorityOut(out string) error {
	refuse := config.Fail("out_not_private", "output must be a writable private directory with private regular files")
	info, err := os.Lstat(out)
	if errors.Is(err, os.ErrNotExist) {
		// Check the nearest existing ancestor before creating anything. Permission
		// bits are checked even for a privileged caller.
		parent := filepath.Dir(out)
		for {
			st, err := os.Stat(parent)
			if errors.Is(err, os.ErrNotExist) {
				parent = filepath.Dir(parent)
				continue
			}
			if err != nil || !st.IsDir() || st.Mode().Perm()&0o300 != 0o300 {
				return refuse
			}
			return nil
		}
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o300 != 0o300 {
		return refuse
	}
	return filepath.WalkDir(out, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return refuse
		}
		st, err := entry.Info()
		if err != nil || st.Mode().Perm()&0o077 != 0 || (!st.IsDir() && (!st.Mode().IsRegular() || st.Mode().Perm()&^config.ModeSecret != 0)) {
			return refuse
		}
		if path == filepath.Join(out, "runtime.json") || path == filepath.Join(out, "authority.json") {
			if !st.Mode().IsRegular() || st.Mode().Perm()&0o177 != 0 {
				return refuse
			}
		}
		if path == filepath.Join(out, "ledger") && (!st.IsDir() || st.Mode().Perm() != config.ModeDir) {
			return refuse
		}
		return nil
	})
}

func writeAuthorityOut(out string, runtimeData, authData []byte) error {
	// Stage both files before publishing either. A new output directory is
	// published with one rename; an existing private directory retains its ledger.
	_, statErr := os.Lstat(out)
	newOut := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !newOut {
		return statErr
	}
	stageParent := out
	if newOut {
		stageParent = filepath.Dir(out)
	}
	if err := os.MkdirAll(stageParent, config.ModeDir); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(stageParent, ".mgw-authority-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	for name, data := range map[string][]byte{"runtime.json": runtimeData, "authority.json": authData} {
		if err := config.WriteFileAtomic(filepath.Join(stage, name), append(data, '\n'), config.ModeSecret); err != nil {
			return err
		}
	}
	if err := os.Mkdir(filepath.Join(stage, "ledger"), config.ModeDir); err != nil {
		return err
	}
	if newOut {
		return os.Rename(stage, out)
	}
	names := []string{"authority.json", "runtime.json"}
	saved := make(map[string]bool)
	for _, name := range names {
		if err := os.Link(filepath.Join(out, name), filepath.Join(stage, name+".old")); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			saved[name] = true
		}
	}
	ledger := filepath.Join(out, "ledger")
	ledgerErr := os.Mkdir(ledger, config.ModeDir)
	if ledgerErr != nil && !errors.Is(ledgerErr, os.ErrExist) {
		return ledgerErr
	}
	var published []string
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, name := range published {
			if saved[name] {
				_ = os.Rename(filepath.Join(stage, name+".old"), filepath.Join(out, name))
			} else {
				_ = os.Remove(filepath.Join(out, name))
			}
		}
		if ledgerErr == nil {
			_ = os.Remove(ledger)
		}
	}()
	for _, name := range names {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(out, name)); err != nil {
			return err
		}
		published = append(published, name)
	}
	committed = true
	return nil
}
