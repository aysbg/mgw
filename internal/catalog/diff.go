package catalog

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Delta is one difference between the curated library and an upstream catalog.
type Delta struct {
	Kind                    string // "new_upstream" | "not_upstream" | "provider_missing"
	Provider                string
	Model                   string
	Context                 int64 // 0 when unknown
	InputMicro, OutputMicro int64 // 0 when the catalog carries no price
}

// Diff compares the curated capability library with an upstream catalog.
// It reports deltas only: it never assigns a tier, a rank or a price.
func Diff(caps *Capabilities, cat *Catalog, minContext int64) ([]Delta, error) {
	seats := make(map[string]map[string]bool)
	for _, seat := range caps.Models {
		model, ok := strings.CutPrefix(seat.ID, seat.Provider+"/")
		if !ok {
			return nil, fmt.Errorf("capabilities: seat %q must start with its provider %q plus /", seat.ID, seat.Provider)
		}
		if seats[seat.Provider] == nil {
			seats[seat.Provider] = make(map[string]bool)
		}
		seats[seat.Provider][model] = true
	}
	deltas := make([]Delta, 0)
	for _, provider := range caps.Providers {
		upstream, exists := cat.Providers[provider.ID]
		for id, m := range upstream.Models {
			if strings.HasPrefix(id, "~") || !AgentCapable(m) || m.ContextLimit < minContext || seats[provider.ID][id] {
				continue
			}
			deltas = append(deltas, Delta{
				Kind: "new_upstream", Provider: provider.ID, Model: id,
				Context: m.ContextLimit, InputMicro: m.InputMicro, OutputMicro: m.OutputMicro,
			})
		}
		for id := range seats[provider.ID] {
			kind := "not_upstream"
			if !exists {
				kind = "provider_missing"
			} else if _, ok := upstream.Models[id]; ok {
				continue
			}
			deltas = append(deltas, Delta{Kind: kind, Provider: provider.ID, Model: id})
		}
	}
	slices.SortFunc(deltas, func(a, b Delta) int {
		return cmp.Or(strings.Compare(a.Provider, b.Provider), strings.Compare(a.Kind, b.Kind), strings.Compare(a.Model, b.Model))
	})
	return deltas, nil
}
