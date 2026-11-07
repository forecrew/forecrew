//go:build enterprise

package ee

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// scimUser is the User resource. Forecrew keeps userName as the email, one
// name, active and externalId; the rest of what providers send is ignored.
type scimUser struct {
	Schemas     []string    `json:"schemas"`
	ID          string      `json:"id,omitempty"`
	ExternalID  string      `json:"externalId,omitempty"`
	UserName    string      `json:"userName"`
	Name        *scimName   `json:"name,omitempty"`
	DisplayName string      `json:"displayName,omitempty"`
	Active      *scimBool   `json:"active,omitempty"`
	Emails      []scimEmail `json:"emails,omitempty"`
	Meta        *scimMeta   `json:"meta,omitempty"`
}

type scimName struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

type scimEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// fullName is the one name Forecrew keeps.
func (u scimUser) fullName() string {
	if u.Name != nil {
		if n := strings.TrimSpace(u.Name.GivenName + " " + u.Name.FamilyName); n != "" {
			return n
		}
		if u.Name.Formatted != "" {
			return u.Name.Formatted
		}
	}
	return u.DisplayName
}

// splitName gives providers back a given and a family name: the last word is
// the family name.
func splitName(full string) (given, family string) {
	i := strings.LastIndexByte(strings.TrimSpace(full), ' ')
	if i < 0 {
		return full, ""
	}
	return full[:i], full[i+1:]
}

// A deleted SCIM user keeps a suspended membership, so that nothing they did
// loses its author, but the resource must be gone (RFC 7644, 3.6). That is a
// membership with source scim and no external ID: every live one has one,
// the user's ID when the provider sends none.
func provisioned(m domain.Membership) bool {
	return m.Source == domain.SourceSCIM && m.ExternalID != ""
}

func externalIDOf(m domain.Membership) string {
	if m.ExternalID == m.UserID.String() {
		return ""
	}
	return m.ExternalID
}

func userResource(c scimCall, u domain.User, m domain.Membership) scimUser {
	active := scimBool(m.Status == domain.MembershipActive)
	given, family := splitName(u.Name)
	return scimUser{
		Schemas: []string{scimUserSchema}, ID: u.ID.String(), ExternalID: externalIDOf(m), UserName: u.Email,
		Name: &scimName{Formatted: u.Name, GivenName: given, FamilyName: family}, DisplayName: u.Name, Active: &active,
		Emails: []scimEmail{{Value: u.Email, Type: "work", Primary: true}},
		Meta:   metaOf("User", c.base+"/Users/"+u.ID.String(), m.CreatedAt, cmp.Or(m.UpdatedAt, m.CreatedAt)),
	}
}

// member finds a member this SCIM token reaches.
func member(ctx context.Context, st store.Store, c scimCall, ref string) (domain.User, domain.Membership, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return domain.User{}, domain.Membership{}, scimNotFound("user")
	}
	m, err := st.Memberships().Get(ctx, c.account, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !provisioned(m)) {
		return domain.User{}, m, scimNotFound("user")
	}
	if err != nil {
		return domain.User{}, m, err
	}
	u, err := st.Users().Get(ctx, id)
	return u, m, err
}

func (s *Service) listUsers(w http.ResponseWriter, r *http.Request, c scimCall) error {
	f, err := parseFilter(r, "username", "externalid")
	if err != nil {
		return err
	}
	ctx := r.Context()
	all, err := s.st.Memberships().List(ctx, c.account, store.MembershipFilter{})
	if err != nil {
		return err
	}
	members := slices.DeleteFunc(all, func(m domain.Membership) bool { return !provisioned(m) })
	ids := make([]uuid.UUID, len(members))
	for i, m := range members {
		ids[i] = m.UserID
	}
	users, err := s.st.Users().ListByIDs(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]domain.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	byCreated(members, func(m domain.Membership) time.Time { return m.CreatedAt }, func(m domain.Membership) uuid.UUID { return m.UserID })
	out := []scimUser{}
	for _, m := range members {
		u := byID[m.UserID]
		switch f.attribute {
		case "username":
			if !strings.EqualFold(u.Email, f.value) {
				continue
			}
		case "externalid":
			if externalIDOf(m) != f.value {
				continue
			}
		}
		out = append(out, userResource(c, u, m))
	}
	return writeSCIM(w, http.StatusOK, page(r, out))
}

func (s *Service) getUser(w http.ResponseWriter, r *http.Request, c scimCall) error {
	u, m, err := member(r.Context(), s.st, c, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeSCIM(w, http.StatusOK, userResource(c, u, m))
}

// checkUserName refuses a userName outside the organization's verified
// domains. Sign-in links people by proven email, and only there can the
// organization's word on an email count (identity.md, Linking by email).
func checkUserName(ctx context.Context, st store.Store, c scimCall, name string) error {
	_, host, ok := strings.Cut(name, "@")
	if !ok || host == "" {
		return scimBad("invalidValue", "userName must be an email address")
	}
	d, err := st.VerifiedDomains().GetVerified(ctx, host)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AccountID != c.account) {
		return scimBad("invalidValue", "%s is not a verified domain of this organization", host)
	}
	return err
}

// checkExternalID refuses an externalId another member of the organization
// has.
func checkExternalID(ctx context.Context, st store.Store, c scimCall, externalID string, self uuid.UUID) error {
	if externalID == "" {
		return nil
	}
	m, err := st.Memberships().GetByExternalID(ctx, c.account, externalID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.UserID != self {
		return scimTaken("externalId %s is taken", externalID)
	}
	return nil
}

func statusOf(active *scimBool) domain.MembershipStatus {
	if active != nil && !bool(*active) {
		return domain.MembershipSuspended
	}
	return domain.MembershipActive
}

// createUser provisions a member. Someone who is a member already, manual,
// invited or by JIT, is a 409: a SCIM token never reaches a member it did not
// provision. An Owner takes them over by removing them first. The one
// exception is a user this endpoint deleted, who comes back.
func (s *Service) createUser(w http.ResponseWriter, r *http.Request, c scimCall) error {
	var in scimUser
	if err := readSCIM(w, r, &in); err != nil {
		return err
	}
	in.UserName = strings.TrimSpace(in.UserName)
	var u domain.User
	var m domain.Membership
	err := s.inTx(r.Context(), func(ctx context.Context, tx store.Store) error {
		if err := checkUserName(ctx, tx, c, in.UserName); err != nil {
			return err
		}
		var err error
		if u, err = tx.Users().GetByVerifiedEmail(ctx, in.UserName); errors.Is(err, store.ErrNotFound) {
			u = domain.User{Name: in.fullName(), Email: in.UserName, EmailVerified: true}
			err = tx.Users().Create(ctx, &u)
		}
		if err != nil {
			return err
		}
		if err := checkExternalID(ctx, tx, c, in.ExternalID, u.ID); err != nil {
			return err
		}
		external := cmp.Or(in.ExternalID, u.ID.String())
		m, err = tx.Memberships().Get(ctx, c.account, u.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			m = domain.Membership{AccountID: c.account, UserID: u.ID, Status: statusOf(in.Active), Source: domain.SourceSCIM, ExternalID: external}
			if err := tx.Memberships().Create(ctx, &m); err != nil {
				return err
			}
		case err != nil:
			return err
		case m.Source == domain.SourceSCIM && m.ExternalID == "":
			if err := tx.Memberships().SetStatus(ctx, c.account, u.ID, statusOf(in.Active)); err != nil {
				return err
			}
			if err := tx.Memberships().SetExternalID(ctx, c.account, u.ID, external); err != nil {
				return err
			}
			m.Status, m.ExternalID = statusOf(in.Active), external
		default:
			return scimTaken("%s is a member already", in.UserName)
		}
		return audit(ctx, tx, c.by, "scim.user_created", "user", u.ID, map[string]any{"token": c.token, "status": m.Status})
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", c.base+"/Users/"+u.ID.String())
	return writeSCIM(w, http.StatusCreated, userResource(c, u, m))
}

// userChange is what a PUT or PATCH leaves a user as. Name is empty to keep
// it.
type userChange struct {
	userName, name, externalID string
	given, family              *string
	active                     bool
}

func changeOf(u domain.User, m domain.Membership) userChange {
	return userChange{userName: u.Email, name: u.Name, externalID: externalIDOf(m), active: m.Status == domain.MembershipActive}
}

// resolved returns the name with the given and family names applied.
func (ch userChange) resolved() string {
	if ch.given == nil && ch.family == nil {
		return ch.name
	}
	given, family := splitName(ch.name)
	if ch.given != nil {
		given = *ch.given
	}
	if ch.family != nil {
		family = *ch.family
	}
	return strings.TrimSpace(given + " " + family)
}

// endAccess suspends a member and, in the same transaction, ends their
// sessions in the organization and revokes their tokens and OAuth grants.
func endAccess(ctx context.Context, tx store.Store, accountID, userID uuid.UUID) error {
	if err := tx.Memberships().SetStatus(ctx, accountID, userID, domain.MembershipSuspended); err != nil {
		return err
	}
	if err := tx.Sessions().RevokeByUser(ctx, userID, accountID); err != nil {
		return err
	}
	if err := tx.MCPTokens().RevokeByOwner(ctx, accountID, domain.SubjectUser, userID); err != nil {
		return err
	}
	return tx.OAuth().RevokeByUser(ctx, accountID, userID)
}

// saveUser writes what changed, within the transaction of the caller.
func saveUser(ctx context.Context, tx store.Store, c scimCall, u *domain.User, m *domain.Membership, ch userChange) error {
	name := ch.resolved()
	if !strings.EqualFold(ch.userName, u.Email) {
		if err := checkUserName(ctx, tx, c, ch.userName); err != nil {
			return err
		}
	}
	if ch.userName != u.Email || name != u.Name {
		u.Email, u.Name = ch.userName, name
		if err := tx.Users().Update(ctx, u); err != nil {
			return err
		}
	}
	if ch.externalID != externalIDOf(*m) {
		if err := checkExternalID(ctx, tx, c, ch.externalID, u.ID); err != nil {
			return err
		}
		m.ExternalID = cmp.Or(ch.externalID, u.ID.String())
		if err := tx.Memberships().SetExternalID(ctx, c.account, u.ID, m.ExternalID); err != nil {
			return err
		}
	}
	wasActive := m.Status == domain.MembershipActive
	switch {
	case wasActive && !ch.active:
		if err := endAccess(ctx, tx, c.account, u.ID); err != nil {
			return err
		}
		m.Status = domain.MembershipSuspended
		return audit(ctx, tx, c.by, "scim.user_deactivated", "user", u.ID, map[string]any{"token": c.token})
	case !wasActive && ch.active:
		if err := tx.Memberships().SetStatus(ctx, c.account, u.ID, domain.MembershipActive); err != nil {
			return err
		}
		m.Status = domain.MembershipActive
	}
	return audit(ctx, tx, c.by, "scim.user_updated", "user", u.ID, map[string]any{"token": c.token})
}

// updateUser loads the user, applies change and answers with the result.
func (s *Service) updateUser(w http.ResponseWriter, r *http.Request, c scimCall, change func(*userChange) error) error {
	var u domain.User
	var m domain.Membership
	err := s.inTx(r.Context(), func(ctx context.Context, tx store.Store) error {
		var err error
		if u, m, err = member(ctx, tx, c, r.PathValue("id")); err != nil {
			return err
		}
		ch := changeOf(u, m)
		if err := change(&ch); err != nil {
			return err
		}
		return saveUser(ctx, tx, c, &u, &m, ch)
	})
	if err != nil {
		return err
	}
	return writeSCIM(w, http.StatusOK, userResource(c, u, m))
}

func (s *Service) replaceUser(w http.ResponseWriter, r *http.Request, c scimCall) error {
	var in scimUser
	if err := readSCIM(w, r, &in); err != nil {
		return err
	}
	return s.updateUser(w, r, c, func(ch *userChange) error {
		if in.UserName == "" {
			return scimBad("invalidValue", "userName is required")
		}
		ch.userName, ch.name, ch.externalID = strings.TrimSpace(in.UserName), cmp.Or(in.fullName(), ch.name), in.ExternalID
		if in.Active != nil {
			ch.active = bool(*in.Active)
		}
		return nil
	})
}

func (s *Service) patchUser(w http.ResponseWriter, r *http.Request, c scimCall) error {
	var body patchBody
	if err := body.read(w, r, scimUserSchema); err != nil {
		return err
	}
	return s.updateUser(w, r, c, func(ch *userChange) error {
		for _, o := range body.Operations {
			if err := applyUserOp(ch, o); err != nil {
				return err
			}
		}
		return nil
	})
}

// applyUserOp applies one operation. Without a path the value is an object of
// attributes, which is how Okta deactivates. Attributes Forecrew does not keep
// (emails among them: userName is the email) are ignored, because providers
// send many and would stop at an error.
func applyUserOp(ch *userChange, o patchOp) error {
	if o.Path != "" {
		return setUserAttribute(ch, o.Op, o.Path, o.Value)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(o.Value, &values); err != nil {
		return scimBad("invalidValue", "without a path the value must be an object")
	}
	for k, v := range values {
		if err := setUserAttribute(ch, o.Op, k, v); err != nil {
			return err
		}
	}
	return nil
}

func setUserAttribute(ch *userChange, op, path string, raw json.RawMessage) error {
	attribute := strings.ToLower(path)
	if op == "remove" {
		switch attribute {
		case "externalid":
			ch.externalID = ""
		case "active", "username":
			return scimBad("mutability", "%s cannot be removed", path)
		}
		return nil
	}
	text := func(out *string) error {
		if err := json.Unmarshal(raw, out); err != nil {
			return scimBad("invalidValue", "%s must be a string", path)
		}
		return nil
	}
	switch attribute {
	case "active":
		var v scimBool
		if err := json.Unmarshal(raw, &v); err != nil {
			return scimBad("invalidValue", "active must be a boolean")
		}
		ch.active = bool(v)
	case "username":
		return text(&ch.userName)
	case "externalid":
		return text(&ch.externalID)
	case "displayname", "name.formatted":
		return text(&ch.name)
	case "name.givenname":
		ch.given = new(string)
		return text(ch.given)
	case "name.familyname":
		ch.family = new(string)
		return text(ch.family)
	case "name":
		var n scimName
		if err := json.Unmarshal(raw, &n); err != nil {
			return scimBad("invalidValue", "name must be an object")
		}
		ch.name = cmp.Or(scimUser{Name: &n}.fullName(), ch.name)
	}
	return nil
}

// deleteUser suspends the member, ends their access and takes them out of the
// groups of this token. Their membership stays, without the resource.
func (s *Service) deleteUser(w http.ResponseWriter, r *http.Request, c scimCall) error {
	err := s.inTx(r.Context(), func(ctx context.Context, tx store.Store) error {
		u, m, err := member(ctx, tx, c, r.PathValue("id"))
		if err != nil {
			return err
		}
		if err := endAccess(ctx, tx, c.account, u.ID); err != nil {
			return err
		}
		if err := tx.Memberships().SetExternalID(ctx, c.account, u.ID, ""); err != nil {
			return err
		}
		teams, err := tx.Teams().TeamsOf(ctx, c.account, u.ID)
		if err != nil {
			return err
		}
		for _, t := range teams {
			if err := removeSCIMMember(ctx, tx, c.account, t.ID, u.ID); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c.by, "scim.user_deleted", "user", u.ID, map[string]any{"token": c.token, "status_was": m.Status})
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
