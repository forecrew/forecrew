package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/store"
)

// newSetupLinkCommand makes forecrew setup-link, the first-owner step of an
// install: it works on the database with shell access to the server, like
// forecrew user add, and prints a link that makes the first Owner of an
// organization once.
func newSetupLinkCommand(env Env) *cobra.Command {
	var org, configPath, baseURL string
	ttl := auth.DefaultSetupLinkTTL
	cmd := &cobra.Command{
		Use:   "setup-link",
		Short: "Print a one-time link that makes the first Owner of an organization",
		Long: "Print a link that works once and for a short time. Whoever opens it and signs in becomes an Owner of the organization.\n" +
			"The token is in the part of the link after #, which browsers do not send to the server. Nothing keeps it on the server but its hash.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if baseURL == "" {
				cfg, err := config.Load(configPath, env.Getenv)
				if err != nil {
					return fmt.Errorf("find the public URL (pass --url or --config): %w", err)
				}
				baseURL = cfg.Server.PublicBaseURL
			}
			return withDatabase(env, cmd, func(ctx context.Context, db Database) error {
				link, err := setupLink(ctx, env, db.Store(), org, baseURL, ttl)
				if err != nil {
					return err
				}
				cmd.PrintErrf("Open this link within %s and sign in. It works once.\n", ttl)
				_, err = fmt.Fprintln(cmd.OutOrStdout(), link)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&org, "org", "default", "slug of the organization")
	cmd.Flags().DurationVar(&ttl, "ttl", ttl, "how long the link works, from a minute to 24 hours")
	cmd.Flags().StringVar(&configPath, "config", "forecrew.yaml", "path to forecrew.yaml, which names the public URL")
	cmd.Flags().StringVar(&baseURL, "url", "", "public URL of the server, instead of the one in --config")
	return cmd
}

// setupLink stores a link for the organization and returns its address. The
// audit entry names the link, never its token.
func setupLink(ctx context.Context, env Env, st store.Store, org, baseURL string, ttl time.Duration) (string, error) {
	acc, err := st.Accounts().GetBySlug(ctx, org)
	if err != nil {
		return "", fmt.Errorf("organization %s: %w", org, err)
	}
	by := operator(env, acc.ID)
	var token string
	err = st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		// A link only ever makes the first Owner (claimSetupLink checks again at sign-in).
		if err := tx.Accounts().Lock(ctx, acc.ID); err != nil {
			return err
		}
		if open, err := tx.SetupLinks().FirstOwnerOpen(ctx, acc.ID); err != nil {
			return err
		} else if !open {
			return fmt.Errorf("organization %s already has an owner or members: a setup link only makes the first owner (invite people with forecrew invitations create, or use forecrew user add)", org)
		}
		var link store.SetupLink
		if token, link, err = auth.CreateSetupLink(ctx, tx, acc.ID, by.String(), ttl, time.Now()); err != nil {
			return err
		}
		return adminAudit(ctx, tx, by, "setup_link.created", "setup_link", link.ID, map[string]any{"ttl": ttl.String()})
	})
	if err != nil {
		return "", fmt.Errorf("create the setup link: %w", err)
	}
	return auth.SetupURL(strings.TrimSpace(baseURL), token), nil
}
