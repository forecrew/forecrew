package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// The server-side break-glass commands (docs/contracts/identity.md,
// Recovery): they work on the database directly and are audited as
// user:<operator>.

const defaultGitHubAPI = "https://api.github.com"

// toScopes converts the values of a --scope flag.
func toScopes(flags []string) []domain.Scope {
	out := make([]domain.Scope, len(flags))
	for i, f := range flags {
		out[i] = domain.Scope(f)
	}
	return out
}

// adminAudit records what the operator did.
func adminAudit(ctx context.Context, st store.Store, by domain.Actor, action, subject string, id uuid.UUID, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal audit data: %w", err)
	}
	return st.Audit().Append(ctx, &domain.AuditEntry{AccountID: by.AccountID, Actor: by.String(), Action: action, SubjectType: subject, SubjectID: id, Data: raw})
}

// operator is the actor of a server command in an organization.
func operator(env Env, accountID uuid.UUID) domain.Actor {
	return domain.Actor{AccountID: accountID, Kind: domain.ActorUser, Name: adminName(env)}
}

// tokenOwner resolves the --user or --service-account of forecrew token
// create. Every new token has an owner (docs/contracts/identity.md, Tokens).
func tokenOwner(ctx context.Context, st store.Store, accountID uuid.UUID, user, serviceAccount string) (domain.SubjectKind, uuid.UUID, error) {
	switch {
	case (user == "") == (serviceAccount == ""):
		return "", uuid.Nil(), errors.New("a token needs an owner: pass --user or --service-account (see forecrew user add)")
	case user != "":
		u, err := findUser(ctx, st, user)
		if err != nil {
			return "", uuid.Nil(), err
		}
		if _, err := st.Memberships().Get(ctx, accountID, u.ID); err != nil {
			return "", uuid.Nil(), fmt.Errorf("%s is not a member of this organization: %w", user, err)
		}
		return domain.SubjectUser, u.ID, nil
	}
	sa, err := st.ServiceAccounts().GetByName(ctx, accountID, serviceAccount)
	if err != nil {
		return "", uuid.Nil(), fmt.Errorf("service account %s: %w", serviceAccount, err)
	}
	return domain.SubjectServiceAccount, sa.ID, nil
}

// createToken stores a token of the owner, audited as the operator.
func createToken(ctx context.Context, env Env, st store.Store, t domain.MCPToken) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.MCPTokens().Create(ctx, &t); err != nil {
			return err
		}
		return adminAudit(ctx, tx, operator(env, t.AccountID), "token.created", "token", t.ID, map[string]any{"name": t.Name, "owner_kind": t.OwnerKind, "owner_id": t.OwnerID.String()})
	})
}

// findUser finds a user by ID, verified email, the only user with an
// unverified email, or the GitHub login of their identity.
func findUser(ctx context.Context, st store.Store, key string) (domain.User, error) {
	if id, err := uuid.Parse(key); err == nil {
		return st.Users().Get(ctx, id)
	}
	if strings.Contains(key, "@") {
		users, err := st.Users().ListByEmail(ctx, key)
		if err != nil {
			return domain.User{}, err
		}
		for _, u := range users {
			if u.EmailVerified {
				return u, nil
			}
		}
		if len(users) == 1 {
			return users[0], nil
		}
		return domain.User{}, fmt.Errorf("no single user has the email %s; use their ID", key)
	}
	conn, err := gitHubConnection(ctx, st)
	if err != nil {
		return domain.User{}, err
	}
	ids, err := st.Identities().ListByLogin(ctx, conn.ID, key)
	if err != nil {
		return domain.User{}, err
	}
	if len(ids) != 1 {
		return domain.User{}, fmt.Errorf("no single user has the GitHub login %s; use their ID", key)
	}
	return st.Users().Get(ctx, ids[0].UserID)
}

// gitHubConnection is the instance GitHub connection, which forecrewd stores
// from auth.connections at startup.
func gitHubConnection(ctx context.Context, st store.Store) (domain.Connection, error) {
	conns, err := st.Connections().List(ctx, uuid.Nil())
	if err != nil {
		return domain.Connection{}, err
	}
	for _, c := range conns {
		if c.Kind == domain.ConnectionGitHub && c.Enabled {
			return c, nil
		}
	}
	return domain.Connection{}, errors.New("no GitHub connection: add one to auth.connections and start forecrewd once")
}

// gitHubUserID returns the stable ID of a login, the subject of the identity.
func gitHubUserID(ctx context.Context, env Env, login string) (string, error) {
	client := env.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	id, err := auth.LookupGitHubUser(ctx, client, cmp.Or(env.GitHubAPI, defaultGitHubAPI), login)
	return strconv.FormatInt(id, 10), err
}

// addUser finds or creates the user of an email or GitHub login. The
// operator vouches for the email, so it is verified and the person links to
// this user at their first sign-in.
func addUser(ctx context.Context, env Env, tx store.Store, key string) (domain.User, error) {
	if strings.Contains(key, "@") {
		u, err := tx.Users().GetByVerifiedEmail(ctx, key)
		if !errors.Is(err, store.ErrNotFound) {
			return u, err
		}
		u = domain.User{Name: key, Email: key, EmailVerified: true}
		return u, tx.Users().Create(ctx, &u)
	}
	conn, err := gitHubConnection(ctx, tx)
	if err != nil {
		return domain.User{}, err
	}
	subject, err := gitHubUserID(ctx, env, key)
	if err != nil {
		return domain.User{}, err
	}
	id, err := tx.Identities().GetBySubject(ctx, conn.ID, subject)
	if err == nil {
		return tx.Users().Get(ctx, id.UserID)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return domain.User{}, err
	}
	u := domain.User{Name: key}
	if err := tx.Users().Create(ctx, &u); err != nil {
		return u, err
	}
	return u, tx.Identities().Create(ctx, &domain.Identity{UserID: u.ID, ConnectionID: conn.ID, Subject: subject, Login: key})
}

func newUserCommand(env Env) *cobra.Command {
	user := &cobra.Command{Use: "user", Short: "Add and disable people on the server"}
	var org, role string
	add := &cobra.Command{
		Use:   "add <email or github-login>",
		Short: "Create a user with a role in an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				if err := userAdd(ctx, env, db.Store(), args[0], org, role); err != nil {
					return err
				}
				cmd.Printf("%s is %s of %s\n", args[0], role, org)
				return nil
			})
		},
	}
	add.Flags().StringVar(&org, "org", "", "slug of the organization")
	add.Flags().StringVar(&role, "role", domain.RoleMember, "organization role")
	_ = add.MarkFlagRequired("org")
	disable := &cobra.Command{
		Use:   "disable <email, github-login or id>",
		Short: "Disable a user: they cannot sign in, and their sessions and tokens stop working",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				if err := userDisable(ctx, env, db.Store(), args[0]); err != nil {
					return fmt.Errorf("disable %s: %w", args[0], err)
				}
				cmd.Printf("%s disabled\n", args[0])
				return nil
			})
		},
	}
	user.AddCommand(add, disable)
	return user
}

// userAdd gives the person of key the role in the organization, and hands
// waiting tokens to a first Owner.
func userAdd(ctx context.Context, env Env, st store.Store, key, org, role string) error {
	acc, err := st.Accounts().GetBySlug(ctx, org)
	if err != nil {
		return fmt.Errorf("organization %s: %w", org, err)
	}
	r, err := st.Roles().GetByName(ctx, acc.ID, domain.RoleScopeOrganization, role)
	if err != nil {
		return fmt.Errorf("role %s: %w", role, err)
	}
	by := operator(env, acc.ID)
	err = st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		u, err := addUser(ctx, env, tx, key)
		if err != nil {
			return err
		}
		if err := auth.Join(ctx, tx, acc.ID, u.ID, domain.SourceManual, []domain.Grant{{RoleID: r.ID}}, by); err != nil {
			return err
		}
		return adminAudit(ctx, tx, by, "user.added", "user", u.ID, map[string]any{"role": role})
	})
	if err != nil {
		return fmt.Errorf("add %s: %w", key, err)
	}
	return auth.AssignTokens(ctx, st)
}

// userDisable disables the user of key and ends their sessions and OAuth
// grants.
func userDisable(ctx context.Context, env Env, st store.Store, key string) error {
	return st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		u, err := findUser(ctx, tx, key)
		if err != nil {
			return err
		}
		if err := tx.Users().SetDisabled(ctx, u.ID, true); err != nil {
			return err
		}
		if err := tx.Sessions().RevokeByUser(ctx, u.ID, uuid.Nil()); err != nil {
			return err
		}
		if err := tx.OAuth().RevokeByUser(ctx, uuid.Nil(), u.ID); err != nil {
			return err
		}
		return auditInOrganizations(ctx, env, tx, u.ID, "user.disabled")
	})
}

// auditInOrganizations records the action in every organization of the user.
func auditInOrganizations(ctx context.Context, env Env, tx store.Store, userID uuid.UUID, action string) error {
	memberships, err := tx.Memberships().ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, m := range memberships {
		if err := adminAudit(ctx, tx, operator(env, m.AccountID), action, "user", userID, nil); err != nil {
			return err
		}
	}
	return nil
}

func newOrgCommand(env Env, r *remote) *cobra.Command {
	org := &cobra.Command{Use: "org", Short: "Create organizations on the server, or change yours"}
	var slug string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an organization; add its first owner with forecrew user add",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				if slug != "" && !domain.ValidSlug(slug) {
					return fmt.Errorf("slug %q must be lowercase letters, digits and -", slug)
				}
				a := domain.Account{Name: args[0], Slug: slug, CreatedBy: operator(env, uuid.Nil()).String()}
				err := db.Store().WithTx(ctx, func(ctx context.Context, tx store.Store) error {
					if err := tx.Accounts().Create(ctx, &a); err != nil {
						return err
					}
					return adminAudit(ctx, tx, operator(env, a.ID), "organization.created", "organization", a.ID, map[string]any{"slug": a.Slug})
				})
				if err != nil {
					return createError("organization", args[0], err)
				}
				cmd.Printf("organization %s created with slug %s\n", a.Name, a.Slug)
				return nil
			})
		},
	}
	create.Flags().StringVar(&slug, "slug", "", "slug in URLs; derived from the name by default")
	org.AddCommand(create, orgUpdateCommand(r), orgSwitchCommand(r))
	return org
}

func newSessionCommand(env Env) *cobra.Command {
	session := &cobra.Command{Use: "session", Short: "End sessions on the server"}
	var user string
	revoke := &cobra.Command{
		Use:   "revoke",
		Short: "End every session of a user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				err := db.Store().WithTx(ctx, func(ctx context.Context, tx store.Store) error {
					u, err := findUser(ctx, tx, user)
					if err != nil {
						return err
					}
					if err := tx.Sessions().RevokeByUser(ctx, u.ID, uuid.Nil()); err != nil {
						return err
					}
					return auditInOrganizations(ctx, env, tx, u.ID, "session.revoked")
				})
				if err != nil {
					return fmt.Errorf("revoke sessions of %s: %w", user, err)
				}
				cmd.Printf("sessions of %s ended\n", user)
				return nil
			})
		},
	}
	revoke.Flags().StringVar(&user, "user", "", "email, GitHub login or ID of the user")
	_ = revoke.MarkFlagRequired("user")
	session.AddCommand(revoke)
	return session
}
