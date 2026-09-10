// Command generate refreshes the embedded models.dev snapshot: it reads a
// full models.dev api.json (the committed fixture by default, the live URL
// behind -live), keeps only the providers the importers know with only the
// per-token cost fields, and writes models.json plus the snapshot date.
//
// The committed snapshot is generated from the committed fixture, never from
// the live network, so CI can reproduce it byte-for-byte offline:
//
//	go generate ./internal/pricing
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

var providers = []string{
	"anthropic",
	"openai",
	"amazon-bedrock",
	"google-vertex-anthropic",
	"openrouter",
	"opencode",
}

// costKeys are the per-token USD-per-1M fields the snapshot keeps. Tiered and
// contextual overrides (tiers, context_over_200k) and modality rates
// (input_audio, output_audio) are dropped: Rates prices the base tier only.
var costKeys = []string{"input", "output", "cache_read", "cache_write", "reasoning"}

func main() {
	in := flag.String("in", "testdata/models-dev-full.json", "full models.dev api.json to trim")
	models := flag.String("models", "models.json", "trimmed snapshot to write")
	snapshot := flag.String("snapshot", "snapshot.go", "generated Go file carrying the snapshot date")
	date := flag.String("date", time.Now().UTC().Format("2006-01-02"), "snapshot date YYYY-MM-DD")
	live := flag.Bool("live", false, "fetch the full file from models.dev instead of -in")
	flag.Parse()

	var raw []byte
	var err error
	if *live {
		raw, err = fetchLive()
	} else {
		raw, err = os.ReadFile(*in)
	}
	if err != nil {
		fatal(err)
	}
	trimmed, err := trim(raw)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*models, append(trimmed, '\n'), 0o644); err != nil {
		fatal(err)
	}
	stub := fmt.Sprintf(`package pricing

// SnapshotDate is the models.dev snapshot the embedded prices were trimmed
// from, in YYYY-MM-DD. Written by go generate; do not edit by hand. Sessions
// show footnotes this date: prices are current rates, not historical ones.
const SnapshotDate = %q
`, *date)
	if err := os.WriteFile(*snapshot, []byte(stub), 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "generate: "+err.Error())
	os.Exit(1)
}

func fetchLive() ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get("https://models.dev/api.json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev answered %s", resp.Status)
	}
	// Capped like the refresh path: a runaway body fails here, in a dev
	// command, instead of filling memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 32<<20 {
		return nil, fmt.Errorf("models.dev body over 32 MB, refusing")
	}
	return raw, nil
}

// trim keeps only the known providers with only the numeric per-token cost
// fields. Providers and models sort themselves: encoding/json emits map keys
// in order, so the output is deterministic.
func trim(raw []byte) ([]byte, error) {
	var full map[string]struct {
		Models map[string]struct {
			Cost map[string]json.RawMessage `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, fmt.Errorf("parse full file: %w", err)
	}
	keep := make(map[string]bool, len(providers))
	for _, p := range providers {
		keep[p] = true
	}
	names := make([]string, 0, len(full))
	for name := range full {
		if keep[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make(map[string]map[string]map[string]float64, len(names))
	for _, name := range names {
		models := make(map[string]map[string]float64)
		for id, m := range full[name].Models {
			cost := make(map[string]float64)
			for _, k := range costKeys {
				raw, ok := m.Cost[k]
				if !ok {
					continue
				}
				var v float64
				if err := json.Unmarshal(raw, &v); err != nil {
					continue
				}
				cost[k] = v
			}
			if len(cost) == 0 {
				continue
			}
			models[id] = cost
		}
		if len(models) == 0 {
			continue
		}
		out[name] = models
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return enc, nil
}
