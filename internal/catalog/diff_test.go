package catalog

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDiff(t *testing.T) {
	raw, err := os.ReadFile("testdata/upstream-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	base := []Delta{
		{Kind: "new_upstream", Provider: "ollama-cloud", Model: "alpha", Context: 100000, InputMicro: 150000, OutputMicro: 600000},
		{Kind: "new_upstream", Provider: "ollama-cloud", Model: "zeta", Context: 200000},
		{Kind: "new_upstream", Provider: "openrouter", Model: "moonshotai/kimi-k3.1-pro", Context: 256000, InputMicro: 500000, OutputMicro: 2000000},
	}
	for _, tt := range []struct {
		name   string
		min    int64
		change func(*Capabilities, *Catalog)
		want   []Delta
	}{
		{name: "new models only, scoped, capable, no aliases, one prefix removed", min: 100000, want: base},
		{name: "higher context threshold", min: 150000, want: base[1:]},
		{name: "empty diff", min: 300000, want: []Delta{}},
		{name: "seat absent upstream", min: 100000, change: func(_ *Capabilities, c *Catalog) {
			delete(c.Providers["ollama-cloud"].Models, "kimi-k3")
		}, want: []Delta{base[0], base[1], {Kind: "not_upstream", Provider: "ollama-cloud", Model: "kimi-k3"}, base[2]}},
		{name: "provider missing upstream", min: 100000, change: func(_ *Capabilities, c *Catalog) {
			delete(c.Providers, "anthropic")
		}, want: []Delta{
			{Kind: "provider_missing", Provider: "anthropic", Model: "claude-haiku-5-5"},
			{Kind: "provider_missing", Provider: "anthropic", Model: "claude-opus-5-5"},
			{Kind: "provider_missing", Provider: "anthropic", Model: "claude-sonnet-5-5"},
			base[0], base[1], base[2],
		}},
		{name: "sort provider kind model", min: 100000, change: func(caps *Capabilities, _ *Catalog) {
			caps.Providers = append(caps.Providers, CapProvider{ID: "zzz"}, CapProvider{ID: "aaa"})
			caps.Models = append(caps.Models,
				Capability{ID: "zzz/last", Provider: "zzz"},
				Capability{ID: "ollama-cloud/gone-z", Provider: "ollama-cloud"},
				Capability{ID: "ollama-cloud/gone-a", Provider: "ollama-cloud"},
				Capability{ID: "aaa/first", Provider: "aaa"})
		}, want: []Delta{
			{Kind: "provider_missing", Provider: "aaa", Model: "first"}, base[0], base[1],
			{Kind: "not_upstream", Provider: "ollama-cloud", Model: "gone-a"},
			{Kind: "not_upstream", Provider: "ollama-cloud", Model: "gone-z"}, base[2],
			{Kind: "provider_missing", Provider: "zzz", Model: "last"},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			caps, err := LoadCapabilities()
			if err != nil {
				t.Fatal(err)
			}
			c, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			if tt.change != nil {
				tt.change(caps, c)
			}
			before, err := json.Marshal([]any{caps, c})
			if err != nil {
				t.Fatal(err)
			}
			for range 10 {
				got, err := Diff(caps, c, tt.min)
				if err != nil || !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("Diff = %+v, %v; want %+v", got, err, tt.want)
				}
			}
			after, err := json.Marshal([]any{caps, c})
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("Diff mutated an input")
			}
		})
	}
}

func TestDiffRejectsMalformedSeat(t *testing.T) {
	for _, id := range []string{"gpt-6-astra", "openai/gpt-6-astra", "openrouterish/gpt-6-astra"} {
		t.Run(id, func(t *testing.T) {
			caps := &Capabilities{Providers: []CapProvider{{ID: "openrouter"}}, Models: []Capability{{ID: id, Provider: "openrouter"}}}
			got, err := Diff(caps, &Catalog{}, 100000)
			if got != nil || err == nil || !strings.Contains(err.Error(), id) {
				t.Fatalf("Diff = %+v, %v; want nil and a seat load error", got, err)
			}
		})
	}
}
