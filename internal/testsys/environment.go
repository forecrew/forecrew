package testsys

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/schema"
)

// EnvironmentsDir is where a repo keeps its environments.
const EnvironmentsDir = ".forecrew/environments"

// environmentFile is the YAML shape of an environment file.
type environmentFile struct {
	Version int `json:"version"`
	domain.Environment
}

// ParseEnvironment validates an environment file and returns it with its
// needs normalized. file is its path or base name; the environment's name
// must equal the base name without .yaml. Every problem wraps ErrInvalid.
func ParseEnvironment(file string, data []byte) (domain.Environment, error) {
	var f environmentFile
	err := validate("environment.schema.json", environmentSchema, data,
		func(raw json.RawMessage) error { return schema.Strict(raw, &f) },
		func() error { return checkNamed(file, f.Name, f.Validate()) })
	if err != nil {
		return domain.Environment{}, fmt.Errorf("%s: %w", file, err)
	}
	e := f.Environment
	e.Needs = domain.NormalizeLabels(e.Needs)
	return e, nil
}

// checkNamed adds to the result of a Validate method the rule that a file
// holds the item named like it (<name>.yaml).
func checkNamed(file, name string, validate error) error {
	var p []string
	if stem := strings.TrimSuffix(path.Base(file), ".yaml"); name != stem {
		p = append(p, fmt.Sprintf("name %q must match the file name %q", name, stem))
	}
	if validate != nil {
		p = append(p, validate.Error())
	}
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(p, "; "))
}
