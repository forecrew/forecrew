// Package councils holds the council contracts and packs: the decision
// brief a council ends with, the standard councils of packs/councils, and
// the prompts and schemas of each council step (docs/councils.md). The
// council runner (T33) builds on it.
package councils

import (
	"encoding/json"
	"fmt"
	"path"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/schema"
)

// BriefSchemaFile is the contract of the brief; packs/schemas/council-brief.json
// is a copy that role packs use as the synthesis output schema.
const BriefSchemaFile = "docs/contracts/council-brief.schema.json"

// ErrInvalid is wrapped by every error about a brief or another council
// output.
var ErrInvalid = schema.ErrInvalid

// ParseBrief checks data, a brief as JSON, against council-brief.schema.json
// and domain.CouncilBrief.Validate, and returns it.
func ParseBrief(data []byte) (domain.CouncilBrief, error) {
	inst, err := schema.DecodeJSON(data)
	if err != nil {
		return domain.CouncilBrief{}, err
	}
	if err := schema.CheckContract(path.Base(BriefSchemaFile), inst); err != nil {
		return domain.CouncilBrief{}, fmt.Errorf("council brief: %w", err)
	}
	var b domain.CouncilBrief
	if err := json.Unmarshal(data, &b); err != nil {
		return domain.CouncilBrief{}, fmt.Errorf("%w: council brief: %w", ErrInvalid, err)
	}
	if err := b.Validate(); err != nil {
		return domain.CouncilBrief{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return b, nil
}
