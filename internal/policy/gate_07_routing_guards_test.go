package policy

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Gate 7, static half: the guards that can be proven from the registry alone.
// The runtime half (writer family exclusion, risk requirement, tool grade,
// budget, pin match at request time) belongs to P5.

// TestGate07_RankZeroSeatCannotBeAllowed: a rank of 0 marks a role ineligible,
// so a seat that lists such a role in roles_allowed is a validation error.
func TestGate07_RankZeroSeatCannotBeAllowed(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	var kimi *Model
	for i := range r.Models {
		if r.Models[i].ID == "ollama-cloud/kimi-k3" {
			kimi = &r.Models[i]
		}
	}
	if kimi == nil || kimi.RoleRank["build"] != 0 {
		t.Fatal("fixture: kimi must have build rank 0")
	}
	kimi.RolesAllowed = append(kimi.RolesAllowed, "build")
	if err := Validate(r); !errors.Is(err, ErrRolesAllowed) {
		t.Fatalf("err = %v, want ErrRolesAllowed", err)
	}
}

// TestGate07_RankedSeatMustBeAllowed is the converse: a non-zero rank on a role
// missing from roles_allowed.
func TestGate07_RankedSeatMustBeAllowed(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"})
	r.Models[0].RolesAllowed = []string{}
	err := Validate(r)
	if !errors.Is(err, ErrRolesAllowed) || !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("err = %v", err)
	}
}

// TestGate07_RankZeroSeatNeverRouted: eligibility is enforced by generation
// too, even when validation was skipped.
func TestGate07_RankZeroSeatNeverRouted(t *testing.T) {
	r := buildFixture(t, allProviders)
	for k, v := range r.DecisionTable {
		c, _ := ParseCell(k)
		for _, id := range v {
			for _, m := range r.Models {
				if m.ID == id && m.RoleRank[c.Role] == 0 {
					t.Fatalf("%s routes %s with rank 0", k, id)
				}
			}
		}
	}
	// Corrupt a registry so the seat is allowed with rank 0; generation skips it.
	r2 := buildFixture(t, []string{"ollama-cloud"})
	for i := range r2.Models {
		if r2.Models[i].ID == "ollama-cloud/kimi-k3" {
			r2.Models[i].RolesAllowed = append(r2.Models[i].RolesAllowed, "build")
		}
	}
	r2.Regenerate()
	if got := r2.DecisionTable[cell("build", "feature", "M", false, "balanced")]; slices.Contains(got, "ollama-cloud/kimi-k3") {
		t.Fatalf("rank 0 seat routed: %v", got)
	}
}

// TestGate07_DenyThatEmptiesBalancedCellIsReported: the deny list removes every
// seat that can review or advise; --check names the cells and fails, and
// validation names the deny entry as the cause.
func TestGate07_DenyThatEmptiesBalancedCellIsReported(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"}, "ollama-cloud/kimi-k3", "ollama-cloud/glm-5.3")
	key := cell("review", "feature", "M", false, "balanced")
	if got := r.DecisionTable[key]; len(got) != 0 {
		t.Fatalf("%s = %v, want empty", key, got)
	}
	rep := Check(r)
	if !rep.Failed() {
		t.Fatal("Check must fail")
	}
	if !slices.Contains(rep.Failures, key) {
		t.Fatalf("failures do not name %s", key)
	}
	for _, f := range rep.Failures {
		if !strings.Contains(f, "|balanced") || (!strings.HasPrefix(f, "review|") && !strings.HasPrefix(f, "advise|")) {
			t.Fatalf("unexpected failure %s", f)
		}
	}
	if len(rep.Failures) != 2*7*3*2 {
		t.Fatalf("failures = %d, want 84", len(rep.Failures))
	}
	if !slices.Contains(rep.Empty, key) {
		t.Fatal("empty list must include every failure")
	}
	if err := Validate(r); !errors.Is(err, ErrNoEligibleSeat) {
		t.Fatalf("err = %v, want ErrNoEligibleSeat", err)
	}
}

// TestGate07_EmptyNonCriticalCellDoesNotFail: an empty explore cell is named but
// is not a hard failure, and a role that never had a seat is not blamed on deny.
func TestGate07_EmptyNonCriticalCellDoesNotFail(t *testing.T) {
	r := buildFixture(t, []string{"ollama-cloud"}, "ollama-cloud/glm-5.3-flash", "ollama-cloud/deepseek-v4.1-flash:1017", "ollama-cloud/glm-5.3")
	if got := r.DecisionTable[cell("explore", "docs", "S", false, "balanced")]; len(got) != 0 {
		t.Fatalf("explore = %v", got)
	}
	rep := Check(r)
	if len(rep.Empty) == 0 {
		t.Fatal("empty cells not named")
	}
	if rep.Failed() {
		t.Fatalf("unexpected hard failure: %v", rep.Failures)
	}
}

// TestGate07_DenyProviderSelectors: a deny entry may name a provider or one
// model id; an unrelated entry denies nothing.
func TestGate07_DenyProviderSelectors(t *testing.T) {
	r := buildFixture(t, allProviders, "openai", "nobody/nothing")
	for _, v := range r.DecisionTable {
		for _, id := range v {
			if strings.HasPrefix(id, "openai/") {
				t.Fatalf("denied provider routed: %s", id)
			}
		}
	}
	if err := Validate(r); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
