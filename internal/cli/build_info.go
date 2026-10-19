package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aysbg/mgw/internal/parity"
)

// BuildInfo identifies the running binary. Its values come from main's ldflags.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
}

// registryBuild records which binary wrote a particular registry digest. This
// sidecar avoids changing the registry schema or its canonical digest contract.
type registryBuild struct {
	BuildInfo
	SHA256 string `json:"sha256"`
}

const registryBuildSuffix = ".build.json"

func writeRegistryBuild(path, sum string, build BuildInfo) error {
	data, err := json.MarshalIndent(registryBuild{BuildInfo: build, SHA256: sum}, "", "  ")
	if err != nil {
		return err
	}
	return writePublicFile(path+registryBuildSuffix, append(data, '\n'))
}

func knownCommit(commit string) bool {
	return commit != "" && commit != "none" && commit != "unknown"
}

func checkBinary(add func(level, name, detail string), build BuildInfo, registryPath string) {
	add("ok", "binary", fmt.Sprintf("version %s, commit %s, built %s", build.Version, build.Commit, build.BuildTime))
	if !knownCommit(build.Commit) {
		return // Local builds cannot identify a mixed install.
	}
	path := registryPath + registryBuildSuffix
	data, err := readFile(path)
	if err != nil {
		add("warn", "binary-registry", fmt.Sprintf("cannot compare binary commit: %s; run `mgw matrix --write` to record the build pin", err))
		return
	}
	var pin registryBuild
	if err := json.Unmarshal(data, &pin); err != nil || !knownCommit(pin.Commit) {
		add("warn", "binary-registry", "no usable commit in "+path+"; run `mgw matrix --write`")
		return
	}
	registry, err := readFile(registryPath)
	if err != nil || pin.SHA256 != parity.SHA256Hex(registry) {
		add("warn", "binary-registry", "build pin does not identify the current registry at "+registryPath+"; run `mgw matrix --write`")
		return
	}
	// Git can lengthen an abbreviated hash as the repository grows. A full hash
	// and an abbreviation of at least seven characters identify the same commit.
	same := build.Commit == pin.Commit || (len(build.Commit) >= 7 && len(pin.Commit) >= 7 &&
		(strings.HasPrefix(build.Commit, pin.Commit) || strings.HasPrefix(pin.Commit, build.Commit)))
	if !same {
		add("warn", "binary-registry", fmt.Sprintf("mixed install: binary commit %s does not match registry pin commit %s (%s); registry can legitimately outlive the binary", build.Commit, pin.Commit, path))
		return
	}
	add("ok", "binary-registry", "binary commit matches registry pin commit "+pin.Commit)
}
