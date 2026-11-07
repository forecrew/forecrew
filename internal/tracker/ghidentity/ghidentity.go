// Package ghidentity is what the GitHub trackers share: GitHub users as
// tracker users, and the Forecrew user a GitHub account is linked to.
package ghidentity

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tracker"
)

// User converts a GitHub user. The ID is GitHub's numeric one, which a rename
// does not change; a user without one has none here either.
func User(u github.User) tracker.User {
	if u.ID == 0 {
		return tracker.User{Name: u.Login, Bot: u.Bot}
	}
	return tracker.User{ID: strconv.FormatInt(u.ID, 10), Name: u.Login, Bot: u.Bot}
}

// LinkedUser implements tracker.Linker: the member linked to the GitHub
// account by its stable ID, when there is exactly one.
func LinkedUser(ctx context.Context, st store.Store, accountID uuid.UUID, u tracker.User) (uuid.UUID, bool, error) {
	id, err := strconv.ParseInt(u.ID, 10, 64)
	if err != nil || id <= 0 {
		return uuid.Nil(), false, nil
	}
	users, err := st.Directory().GitHubMembers(ctx, accountID, id, "")
	if err != nil {
		return uuid.Nil(), false, fmt.Errorf("find the user of %s: %w", u.Name, err)
	}
	if len(users) != 1 {
		return uuid.Nil(), false, nil
	}
	return users[0], true, nil
}

// OwnLabels are on the issues Forecrew opens, which are never tickets.
var OwnLabels = []string{"forecrew:goal", "forecrew:task", "forecrew:bug"}

// HasLabel reports whether labels holds want, ignoring case.
func HasLabel(labels []string, want string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(l, want) })
}

// IsOwn reports whether an issue with these labels is one Forecrew opened.
func IsOwn(labels []string) bool {
	return slices.ContainsFunc(OwnLabels, func(l string) bool { return HasLabel(labels, l) })
}

// TicketType is the issue's type, or bug for the label of that name on a repo
// without issue types.
func TicketType(issueType string, labels []string) string {
	if issueType == "" && HasLabel(labels, "bug") {
		return "bug"
	}
	return issueType
}
