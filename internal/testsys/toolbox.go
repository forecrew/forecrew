package testsys

import (
	"encoding/json"
	"fmt"

	"github.com/forecrew/forecrew/internal/domain"
)

// ParseToolbox validates a packs/toolboxes/<name>.yaml file and returns it
// with its needs normalized. The toolbox's name must equal the file's base
// name without .yaml. Every problem wraps ErrInvalid.
func ParseToolbox(file string, data []byte) (domain.Toolbox, error) {
	var t domain.Toolbox
	err := validate("toolbox.schema.json", toolboxSchema, data,
		func(raw json.RawMessage) error { return decodeJSON(raw, &t) },
		func() error { return checkNamed(file, t.Name, t.Validate()) })
	if err != nil {
		return domain.Toolbox{}, fmt.Errorf("%s: %w", file, err)
	}
	t.Needs = domain.NormalizeLabels(t.Needs)
	return t, nil
}
