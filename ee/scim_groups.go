//go:build enterprise

package ee

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// scimGroup is the Group resource: a team with source scim. Members are those
// this endpoint added; a team's manual and idp members are not its business.
type scimGroup struct {
	Schemas     []string     `json:"schemas"`
	ID          string       `json:"id,omitempty"`
	ExternalID  string       `json:"externalId,omitempty"`
	DisplayName string       `json:"displayName"`
	Members     []scimMember `json:"members,omitzero"`
	Meta        *scimMeta    `json:"meta,omitempty"`
}

// scimMember is a member of a group. Entra ID sends "$ref": null.
type scimMember struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

func (s *Service) groupResource(ctx context.Context, st store.Store, c scimCall, t domain.Team, withMembers bool) (scimGroup, error) {
	g := scimGroup{Schemas: []string{scimGroupSchema}, ID: t.ID.String(), ExternalID: t.ExternalID, DisplayName: t.Name, Members: []scimMember{},
		Meta: metaOf("Group", c.base+"/Groups/"+t.ID.String(), t.CreatedAt, cmp.Or(t.UpdatedAt, t.CreatedAt))}
	if !withMembers {
		g.Members = nil
		return g, nil
	}
	members, err := st.Teams().Members(ctx, c.account, t.ID)
	if err != nil {
		return g, err
	}
	ids := []uuid.UUID{}
	for _, m := range members {
		if m.Source == domain.SourceSCIM {
			ids = append(ids, m.UserID)
		}
	}
	users, err := st.Users().ListByIDs(ctx, ids)
	if err != nil {
		return g, err
	}
	byCreated(users, func(u domain.User) time.Time { return u.CreatedAt }, func(u domain.User) uuid.UUID { return u.ID })
	for _, u := range users {
		g.Members = append(g.Members, scimMember{Value: u.ID.String(), Display: u.Email})
	}
	return g, nil
}

// wantsMembers says whether the request leaves members in, which Entra ID
// asks not to with excludedAttributes.
func wantsMembers(r *http.Request) bool {
	return !strings.Contains(strings.ToLower(r.URL.Query().Get("excludedAttributes")), "members")
}

// group finds a team this SCIM token reaches.
func group(ctx context.Context, st store.Store, c scimCall, ref string) (domain.Team, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return domain.Team{}, scimNotFound("group")
	}
	t, err := st.Teams().Get(ctx, c.account, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.Source != domain.SourceSCIM) {
		return t, scimNotFound("group")
	}
	return t, err
}

func (s *Service) scimTeams(ctx context.Context, st store.Store, c scimCall) ([]domain.Team, error) {
	all, err := st.Teams().List(ctx, c.account)
	if err != nil {
		return nil, err
	}
	teams := slices.DeleteFunc(all, func(t domain.Team) bool { return t.Source != domain.SourceSCIM })
	byCreated(teams, func(t domain.Team) time.Time { return t.CreatedAt }, func(t domain.Team) uuid.UUID { return t.ID })
	return teams, nil
}

func (s *Service) listGroups(w http.ResponseWriter, r *http.Request, c scimCall) error {
	f, err := parseFilter(r, "displayname", "externalid")
	if err != nil {
		return err
	}
	ctx := r.Context()
	teams, err := s.scimTeams(ctx, s.st, c)
	if err != nil {
		return err
	}
	out := []scimGroup{}
	for _, t := range teams {
		if (f.attribute == "displayname" && !strings.EqualFold(t.Name, f.value)) || (f.attribute == "externalid" && t.ExternalID != f.value) {
			continue
		}
		g, err := s.groupResource(ctx, s.st, c, t, wantsMembers(r))
		if err != nil {
			return err
		}
		out = append(out, g)
	}
	return writeSCIM(w, http.StatusOK, page(r, out))
}

func (s *Service) getGroup(w http.ResponseWriter, r *http.Request, c scimCall) error {
	ctx := r.Context()
	t, err := group(ctx, s.st, c, r.PathValue("id"))
	if err != nil {
		return err
	}
	g, err := s.groupResource(ctx, s.st, c, t, wantsMembers(r))
	if err != nil {
		return err
	}
	return writeSCIM(w, http.StatusOK, g)
}

// checkGroup refuses a displayName or externalId another group has.
func (s *Service) checkGroup(ctx context.Context, tx store.Store, c scimCall, self uuid.UUID, name, externalID string) error {
	if strings.TrimSpace(name) == "" {
		return scimBad("invalidValue", "displayName is required")
	}
	teams, err := s.scimTeams(ctx, tx, c)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(teams, func(t domain.Team) bool { return t.ID != self && strings.EqualFold(t.Name, name) }) {
		return scimTaken("a group is called %s", name)
	}
	if externalID == "" {
		return nil
	}
	t, err := tx.Teams().GetByExternalID(ctx, c.account, externalID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.ID != self {
		return scimTaken("externalId %s is taken", externalID)
	}
	return nil
}

// freeSlug derives a slug from the name: the first free -2, -3, and so on, as
// a manual team may have it.
func freeSlug(ctx context.Context, tx store.Store, accountID uuid.UUID, name string) (string, error) {
	base := domain.Slugify(name)
	slug := base
	for n := 2; ; n++ {
		_, err := tx.Teams().GetBySlug(ctx, accountID, slug)
		if errors.Is(err, store.ErrNotFound) {
			return slug, nil
		}
		if err != nil {
			return "", err
		}
		suffix := "-" + strconv.Itoa(n)
		slug = strings.TrimRight(base[:min(len(base), 63-len(suffix))], "-") + suffix
	}
}

// addSCIMMember adds a member this token provisioned to a team. Someone who
// is in it already, by hand say, stays as they are.
func addSCIMMember(ctx context.Context, tx store.Store, c scimCall, teamID uuid.UUID, ref string) error {
	u, _, err := member(ctx, tx, c, ref)
	if e, ok := errors.AsType[*scimError](err); ok && e.status == http.StatusNotFound {
		return scimBad("invalidValue", "member %s is not a user of this endpoint", ref)
	}
	if err != nil {
		return err
	}
	return tx.Teams().AddMember(ctx, &domain.TeamMember{TeamID: teamID, UserID: u.ID, AccountID: c.account, Source: domain.SourceSCIM})
}

// removeSCIMMember takes a user out of a team if SCIM put them there.
func removeSCIMMember(ctx context.Context, tx store.Store, accountID, teamID, userID uuid.UUID) error {
	members, err := tx.Teams().Members(ctx, accountID, teamID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(members, func(m domain.TeamMember) bool { return m.UserID == userID && m.Source == domain.SourceSCIM }) {
		return nil
	}
	return tx.Teams().RemoveMember(ctx, accountID, teamID, userID)
}

// setMembers makes the SCIM members of a team exactly want.
func setMembers(ctx context.Context, tx store.Store, c scimCall, teamID uuid.UUID, want []scimMember) error {
	members, err := tx.Teams().Members(ctx, c.account, teamID)
	if err != nil {
		return err
	}
	for _, m := range members {
		kept := slices.ContainsFunc(want, func(x scimMember) bool { return x.Value == m.UserID.String() })
		if m.Source == domain.SourceSCIM && !kept {
			if err := tx.Teams().RemoveMember(ctx, c.account, teamID, m.UserID); err != nil {
				return err
			}
		}
	}
	for _, x := range want {
		if err := addSCIMMember(ctx, tx, c, teamID, x.Value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) createGroup(w http.ResponseWriter, r *http.Request, c scimCall) error {
	var in scimGroup
	if err := readSCIM(w, r, &in); err != nil {
		return err
	}
	var out scimGroup
	err := s.inTx(r.Context(), func(ctx context.Context, tx store.Store) error {
		if err := s.checkGroup(ctx, tx, c, uuid.Nil(), in.DisplayName, in.ExternalID); err != nil {
			return err
		}
		slug, err := freeSlug(ctx, tx, c.account, in.DisplayName)
		if err != nil {
			return err
		}
		t := domain.Team{AccountID: c.account, Name: in.DisplayName, Slug: slug, Source: domain.SourceSCIM, ExternalID: in.ExternalID}
		if err := tx.Teams().Create(ctx, &t); err != nil {
			return err
		}
		if err := setMembers(ctx, tx, c, t.ID, in.Members); err != nil {
			return err
		}
		if out, err = s.groupResource(ctx, tx, c, t, true); err != nil {
			return err
		}
		return audit(ctx, tx, c.by, "scim.group_created", "team", t.ID, map[string]any{"token": c.token, "name": t.Name})
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", c.base+"/Groups/"+out.ID)
	return writeSCIM(w, http.StatusCreated, out)
}

// updateGroup runs change on the team in a transaction and writes its name
// and external ID.
func (s *Service) updateGroup(r *http.Request, c scimCall, change func(ctx context.Context, tx store.Store, t *domain.Team) error) (domain.Team, error) {
	var t domain.Team
	err := s.inTx(r.Context(), func(ctx context.Context, tx store.Store) error {
		var err error
		if t, err = group(ctx, tx, c, r.PathValue("id")); err != nil {
			return err
		}
		if err := change(ctx, tx, &t); err != nil {
			return err
		}
		if err := s.checkGroup(ctx, tx, c, t.ID, t.Name, t.ExternalID); err != nil {
			return err
		}
		if err := tx.Teams().Update(ctx, &t); err != nil {
			return err
		}
		return audit(ctx, tx, c.by, "scim.group_updated", "team", t.ID, map[string]any{"token": c.token, "name": t.Name})
	})
	return t, err
}

func (s *Service) replaceGroup(w http.ResponseWriter, r *http.Request, c scimCall) error {
	var in scimGroup
	if err := readSCIM(w, r, &in); err != nil {
		return err
	}
	t, err := s.updateGroup(r, c, func(ctx context.Context, tx store.Store, t *domain.Team) error {
		t.Name, t.ExternalID = in.DisplayName, in.ExternalID
		if in.Members == nil {
			return nil
		}
		return setMembers(ctx, tx, c, t.ID, in.Members)
	})
	if err != nil {
		return err
	}
	g, err := s.groupResource(r.Context(), s.st, c, t, true)
	if err != nil {
		return err
	}
	return writeSCIM(w, http.StatusOK, g)
}

var memberPathRE = regexp.MustCompile(`(?i)^members\[\s*value\s+eq\s+"([^"]*)"\s*\]$`)

// patchGroup answers 204, as Entra ID asks: it would not read the members.
func (s *Service) patchGroup(w http.ResponseWriter, r *http.Request, c scimCall) error {
	var body patchBody
	if err := body.read(w, r, scimGroupSchema); err != nil {
		return err
	}
	_, err := s.updateGroup(r, c, func(ctx context.Context, tx store.Store, t *domain.Team) error {
		for _, o := range body.Operations {
			if err := applyGroupOp(ctx, tx, c, t, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func applyGroupOp(ctx context.Context, tx store.Store, c scimCall, t *domain.Team, o patchOp) error {
	if o.Path != "" {
		return setGroupAttribute(ctx, tx, c, t, o.Op, o.Path, o.Value)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(o.Value, &values); err != nil {
		return scimBad("invalidValue", "without a path the value must be an object")
	}
	for k, v := range values {
		if err := setGroupAttribute(ctx, tx, c, t, o.Op, k, v); err != nil {
			return err
		}
	}
	return nil
}

// membersOf reads the members of an operation: a list, or one member.
func membersOf(raw json.RawMessage) ([]scimMember, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []scimMember
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var one scimMember
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, scimBad("invalidValue", "members must be a list of members")
	}
	return []scimMember{one}, nil
}

func setGroupAttribute(ctx context.Context, tx store.Store, c scimCall, t *domain.Team, op, path string, raw json.RawMessage) error {
	if m := memberPathRE.FindStringSubmatch(path); m != nil {
		if op != "remove" {
			return scimBad("invalidPath", "only remove takes a member filter")
		}
		id, err := uuid.Parse(m[1])
		if err != nil {
			return nil // nobody by that value, so nothing to remove
		}
		return removeSCIMMember(ctx, tx, c.account, t.ID, id)
	}
	switch strings.ToLower(path) {
	case "displayname":
		if op == "remove" {
			return scimBad("mutability", "displayName cannot be removed")
		}
		if err := json.Unmarshal(raw, &t.Name); err != nil {
			return scimBad("invalidValue", "displayName must be a string")
		}
	case "externalid":
		t.ExternalID = ""
		if op != "remove" {
			if err := json.Unmarshal(raw, &t.ExternalID); err != nil {
				return scimBad("invalidValue", "externalId must be a string")
			}
		}
	case "members":
		return patchMembers(ctx, tx, c, t.ID, op, raw)
	}
	return nil
}

func patchMembers(ctx context.Context, tx store.Store, c scimCall, teamID uuid.UUID, op string, raw json.RawMessage) error {
	members, err := membersOf(raw)
	if err != nil {
		return err
	}
	switch {
	case op == "replace", op == "remove" && len(members) == 0:
		return setMembers(ctx, tx, c, teamID, members)
	case op == "add":
		for _, m := range members {
			if err := addSCIMMember(ctx, tx, c, teamID, m.Value); err != nil {
				return err
			}
		}
		return nil
	}
	for _, m := range members {
		id, err := uuid.Parse(m.Value)
		if err != nil {
			continue // nobody by that value, so nothing to remove
		}
		if err := removeSCIMMember(ctx, tx, c.account, teamID, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) deleteGroup(w http.ResponseWriter, r *http.Request, c scimCall) error {
	err := s.inTx(r.Context(), func(ctx context.Context, tx store.Store) error {
		t, err := group(ctx, tx, c, r.PathValue("id"))
		if err != nil {
			return err
		}
		if err := tx.Teams().Delete(ctx, c.account, t.ID); err != nil {
			return err
		}
		return audit(ctx, tx, c.by, "scim.group_deleted", "team", t.ID, map[string]any{"token": c.token, "name": t.Name})
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
