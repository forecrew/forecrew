package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// restSources are the sources a REST caller may name; a plan of the assistant
// or of an MCP client comes through those entry points only.
var restSources = []domain.ConfigPlanSource{domain.ConfigFromForm, domain.ConfigFromCLI, domain.ConfigFromYAML}

func restSource(s string) (domain.ConfigPlanSource, error) {
	source := domain.ConfigPlanSource(s)
	if s == "" {
		return domain.ConfigFromForm, nil
	}
	if !slices.Contains(restSources, source) {
		return "", fmt.Errorf("source must be form, cli or yaml, got %q", s)
	}
	return source, nil
}

func configKindInput(r *http.Request) (mcp.ConfigKindInput, error) {
	return mcp.ConfigKindInput{Kind: r.URL.Query().Get("kind")}, nil
}

func configGetInput(r *http.Request) (mcp.ConfigGetInput, error) {
	q := r.URL.Query()
	return mcp.ConfigGetInput{Kind: q.Get("kind"), Name: q.Get("name")}, nil
}

func configPlanInput(r *http.Request) (mcp.ConfigPlanInput, error) {
	var body struct {
		mcp.ConfigPlanInput
		Source string `json:"source"`
	}
	if err := decode(r, &body); err != nil {
		return mcp.ConfigPlanInput{}, err
	}
	var err error
	body.ConfigPlanInput.Source, err = restSource(body.Source)
	return body.ConfigPlanInput, err
}

func configImportInput(r *http.Request) (mcp.ConfigImportInput, error) {
	var in mcp.ConfigImportInput
	err := decode(r, &in)
	in.Source = domain.ConfigFromYAML
	return in, err
}

func configPlansInput(r *http.Request) (mcp.ConfigPlansInput, error) {
	q := r.URL.Query()
	in := mcp.ConfigPlansInput{Inbox: q.Get("inbox") == "true"}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return in, fmt.Errorf("limit: %w", err)
		}
		in.Limit = n
	}
	return in, nil
}

func planRefInput(r *http.Request) (mcp.ConfigPlanRefInput, error) {
	return mcp.ConfigPlanRefInput{Plan: r.PathValue("plan")}, nil
}

// configApplyInput sets Browser from the request itself: no field of the body
// can say that a person confirmed in the web UI.
func configApplyInput(r *http.Request) (mcp.ConfigApplyInput, error) {
	var body struct {
		Word string `json:"word"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			return mcp.ConfigApplyInput{}, err
		}
	}
	return mcp.ConfigApplyInput{Plan: r.PathValue("plan"), Word: body.Word, Browser: fromBrowser(r)}, nil
}

func configSecretInput(r *http.Request) (mcp.ConfigSecretInput, error) {
	var body struct {
		Value string `json:"value"`
	}
	err := decode(r, &body)
	return mcp.ConfigSecretInput{Plan: r.PathValue("plan"), Secret: r.PathValue("secret"), Value: body.Value}, err
}

func assistantListInput(r *http.Request) (mcp.AssistantListInput, error) {
	var in mcp.AssistantListInput
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return in, fmt.Errorf("limit: %w", err)
		}
		in.Limit = n
	}
	return in, nil
}

func assistantRefInput(r *http.Request) (mcp.AssistantRefInput, error) {
	return mcp.AssistantRefInput{Conversation: r.PathValue("conversation")}, nil
}
