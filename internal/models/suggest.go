package models

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

// Presets of a suggestion.
const (
	PresetBalanced  = "balanced"   // the tier each role is written for, then the price
	PresetLowCost   = "low_cost"   // the cheapest model that passed the role's level
	PresetLocalOnly = "local_only" // only models on your own network, as balanced
)

// Presets returns the presets, the default first.
func Presets() []string { return []string{PresetBalanced, PresetLowCost, PresetLocalOnly} }

// RoleNeed is what a suggestion fills for a role: the tier its pack is
// written for, and whether it writes code.
type RoleNeed struct {
	Name   string
	Tier   config.Tier
	Writes bool
}

// Candidate is a model that passed its test on a connection that passed its
// key check.
type Candidate struct {
	Connection string      `json:"connection"`
	Vendor     string      `json:"vendor"`
	Model      string      `json:"model"`
	Maker      string      `json:"maker"`
	Tier       config.Tier `json:"tier"`
	Level      string      `json:"level"`
	Local      bool        `json:"local,omitempty"`
	// Price is input plus output per million tokens; +Inf when unknown.
	Price float64 `json:"-"`
}

// Suggestion is a deterministic proposal of the organization's tiers and
// roles; no model is involved in making it.
type Suggestion struct {
	Preset string                       `json:"preset"`
	Tiers  map[string]map[string]string `json:"tiers"`
	Roles  map[string]domain.ModelRole  `json:"roles"`
	// Notes say, in words, what could not be filled and why.
	Notes []string `json:"notes"`
}

// Candidates lists what a suggestion may use: enabled connections whose key
// check passed, and their models whose test passed at least reading with the
// agent of now. A model's maker and tier come from the catalog, or from what
// people entered on the connection.
func Candidates(cat *Catalog, conns []domain.ModelConnection, checks []domain.ModelCheck) (out []Candidate, notes []string) {
	keyOK := map[string]bool{}
	for _, c := range checks {
		if c.Model == "" && (c.Status == domain.ModelCheckReachable || c.Status == domain.ModelCheckUnsupported) {
			keyOK[c.ConnectionID.String()] = true
		}
	}
	byID := map[string]domain.ModelConnection{}
	for _, c := range conns {
		byID[c.ID.String()] = c
	}
	for _, t := range checks {
		c, ok := byID[t.ConnectionID.String()]
		if t.Model == "" || !ok || !c.Enabled || !keyOK[c.ID.String()] || t.Stale || t.Status != domain.SuitabilityDone || t.Test == nil || t.Test.Level == domain.LevelNone {
			continue
		}
		info := c.Models[t.Model]
		m, known := cat.Model(t.Model)
		tier := config.Tier(cmp.Or(info.Tier, m.Tier))
		if !tier.Valid() {
			notes = append(notes, fmt.Sprintf("Model %s on %s has no tier: enter it on the connection to have it suggested.", t.Model, c.Name))
			continue
		}
		price := math.Inf(1)
		switch {
		case info.Price != nil:
			price = info.Price.Input + info.Price.Output
		case known:
			price = m.Price.Input + m.Price.Output
		}
		local := LocalEndpoint(c.BaseURL)
		if local {
			price = 0
		}
		out = append(out, Candidate{Connection: c.Name, Vendor: c.Vendor, Model: t.Model, Maker: cmp.Or(cat.MakerOf(t.Model), info.Maker, c.Vendor),
			Tier: tier, Level: t.Test.Level, Local: local, Price: price})
	}
	slices.SortFunc(out, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(a.Connection, b.Connection), cmp.Compare(a.Model, b.Model))
	})
	return out, notes
}

// tierRank orders the tiers.
func tierRank(t config.Tier) int {
	return slices.Index([]config.Tier{config.TierFast, config.TierStrong, config.TierBest}, t)
}

// Suggest fills the roles of needs from candidates with preset. The reviewer
// never gets a model of the developer's maker when another maker passed;
// when none did, it is left as it is unless allowSameMaker, and a note says
// so. A tier holds one model per vendor, so a role takes another model of a
// vendor only through another tier.
func Suggest(candidates []Candidate, needs []RoleNeed, preset string, allowSameMaker bool) (Suggestion, error) {
	preset = cmp.Or(preset, PresetBalanced)
	if !slices.Contains(Presets(), preset) {
		return Suggestion{}, fmt.Errorf("preset %q is not one of %v", preset, Presets())
	}
	s := Suggestion{Preset: preset, Tiers: map[string]map[string]string{}, Roles: map[string]domain.ModelRole{}, Notes: []string{}}
	if preset == PresetLocalOnly {
		candidates = slices.DeleteFunc(slices.Clone(candidates), func(c Candidate) bool { return !c.Local })
	}
	if len(candidates) == 0 {
		s.Notes = append(s.Notes, "No model passed its test on a connection that passed its key check"+map[bool]string{true: " on your own network", false: ""}[preset == PresetLocalOnly]+
			": connect a provider, check its key and test a model first.")
		return s, nil
	}
	order := slices.SortedFunc(slices.Values(needs), func(a, b RoleNeed) int {
		// The developer first: the reviewer's maker depends on it.
		first := func(n RoleNeed) int { return map[bool]int{true: 0, false: 1}[n.Name == DeveloperRole] }
		return cmp.Or(cmp.Compare(first(a), first(b)), cmp.Compare(a.Name, b.Name))
	})
	developer := ""
	for _, n := range order {
		best, ok := s.pick(candidates, n, preset, developer, allowSameMaker)
		if !ok {
			continue
		}
		if s.Tiers[string(best.Tier)] == nil {
			s.Tiers[string(best.Tier)] = map[string]string{}
		}
		s.Tiers[string(best.Tier)][best.Vendor] = best.Model
		s.Roles[n.Name] = domain.ModelRole{Connection: best.Connection, Tier: string(best.Tier)}
		if n.Name == DeveloperRole {
			developer = best.Maker
		}
	}
	return s, nil
}

// pick chooses the candidate of role n, or adds a note on why there is none.
// developer is the maker the developer got, if any.
func (s *Suggestion) pick(candidates []Candidate, n RoleNeed, preset, developer string, allowSameMaker bool) (Candidate, bool) {
	eligible := s.eligible(candidates, n)
	if n.Name == ReviewerRole && developer != "" {
		others := slices.DeleteFunc(slices.Clone(eligible), func(c Candidate) bool { return c.Maker == developer })
		if len(others) == 0 && !allowSameMaker {
			s.Notes = append(s.Notes, fmt.Sprintf("Only models of %s passed for the reviewer, the developer's maker, so the reviewer is left as it is: connect a provider of another maker, or set policy.allow_same_vendor_review.", developer))
			return Candidate{}, false
		}
		if len(others) > 0 {
			eligible = others
		}
	}
	if len(eligible) == 0 {
		need := domain.LevelRead
		if n.Writes {
			need = domain.LevelCode
		}
		s.Notes = append(s.Notes, fmt.Sprintf("No tested model serves role %s, which needs level %s: it is left as it is.", n.Name, need))
		return Candidate{}, false
	}
	return slices.MinFunc(eligible, func(a, b Candidate) int { return compare(preset, n, a, b) }), true
}

// eligible are the candidates that serve role n and fit the tiers filled so
// far: a tier holds one model per vendor.
func (s Suggestion) eligible(candidates []Candidate, n RoleNeed) []Candidate {
	return slices.DeleteFunc(slices.Clone(candidates), func(c Candidate) bool {
		slot := s.Tiers[string(c.Tier)][c.Vendor]
		return !domain.Serves(c.Level, n.Writes) || (slot != "" && slot != c.Model)
	})
}

// compare orders two candidates for role n under preset; the first is best.
func compare(preset string, n RoleNeed, a, b Candidate) int {
	dist := func(c Candidate) int {
		d := tierRank(c.Tier) - tierRank(n.Tier)
		if d < 0 {
			return -2*d + 1 // below the role's tier is worse than above
		}
		return 2 * d
	}
	byFit := cmp.Compare(dist(a), dist(b))
	byPrice := cmp.Compare(a.Price, b.Price)
	if preset == PresetLowCost {
		return cmp.Or(byPrice, byFit, cmp.Compare(a.Connection, b.Connection), cmp.Compare(a.Model, b.Model))
	}
	return cmp.Or(byFit, byPrice, cmp.Compare(a.Connection, b.Connection), cmp.Compare(a.Model, b.Model))
}

// Merge puts a suggestion over the organization's assignment: the roles it
// filled move, the others keep their connection and model where their tier
// still holds it.
func Merge(cur domain.ModelAssignment, s Suggestion, conns map[string]domain.ModelConnection) (tiers map[string]map[string]string, roles map[string]domain.ModelRole, notes []string) {
	tiers, roles = map[string]map[string]string{}, map[string]domain.ModelRole{}
	for t, vendors := range s.Tiers {
		tiers[t] = maps.Clone(vendors)
	}
	maps.Copy(roles, s.Roles)
	for _, role := range slices.Sorted(maps.Keys(cur.Roles)) {
		if _, ok := roles[role]; ok {
			continue
		}
		r := cur.Roles[role]
		vendor := conns[r.Connection].Vendor
		model := cur.Tiers[r.Tier][vendor]
		switch slot := tiers[r.Tier][vendor]; {
		case vendor == "" || model == "":
			roles[role] = r // a runtime of the server file, or nothing to keep
		case slot == "" || slot == model:
			if tiers[r.Tier] == nil {
				tiers[r.Tier] = map[string]string{}
			}
			tiers[r.Tier][vendor], roles[role] = model, r
		default:
			notes = append(notes, fmt.Sprintf("Role %s keeps connection %s and moves from model %s to %s, which the suggestion put in tier %s.", role, r.Connection, model, slot, r.Tier))
			roles[role] = r
		}
	}
	return tiers, roles, notes
}

// Roles that separation of duties keeps on models of different makers
// (config-resources.md).
const (
	DeveloperRole = "developer"
	ReviewerRole  = "reviewer"
)
