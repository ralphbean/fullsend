package k8spoc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// Fixture is a credential-free source. A key identifies one observation, not an entity.
type Fixture struct {
	Tenant       string        `json:"tenant"`
	Source       string        `json:"source"`
	Cursor       string        `json:"cursor"`
	Observations []Observation `json:"observations"`
}

type Observation struct {
	Key        string `json:"key"`
	Entity     string `json:"entity"`
	Revision   string `json:"revision"`
	Kind       string `json:"kind"`
	Behavior   string `json:"behavior"`
	Authorized bool   `json:"authorized"`
}

// Selection is fixture-only policy. Live selection will use the existing CEL contract.
type Selection struct {
	ByKind map[string][]string `json:"by_kind"`
}

func readFixture(path string) (Fixture, error) {
	var f Fixture
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err = json.Unmarshal(b, &f); err != nil {
		return f, err
	}
	if f.Tenant == "" || f.Source == "" || f.Cursor == "" {
		return f, fmt.Errorf("fixture requires tenant, source and cursor")
	}
	seen := map[string]bool{}
	for _, o := range f.Observations {
		if o.Key == "" || o.Entity == "" || o.Revision == "" || o.Kind == "" {
			return f, fmt.Errorf("observation requires key, entity, revision and kind")
		}
		if seen[o.Key] {
			return f, fmt.Errorf("duplicate observation key %q", o.Key)
		}
		seen[o.Key] = true
		switch o.Behavior {
		case "success", "failure", "active":
		default:
			return f, fmt.Errorf("invalid behavior %q", o.Behavior)
		}
	}
	return f, nil
}

func readSelection(path string) ([]byte, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var s Selection
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, "", err
	}
	if s.ByKind == nil {
		return nil, "", fmt.Errorf("selection requires by_kind")
	}
	for _, names := range s.ByKind {
		for _, name := range names {
			if name == "" {
				return nil, "", fmt.Errorf("empty harness name")
			}
		}
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), nil
}
