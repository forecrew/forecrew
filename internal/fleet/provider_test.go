package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

// fake shows that a Provider is implementable with the types of the
// contract; the real fake lives in internal/testkit (T58).
type fake struct{}

func (fake) Capabilities(context.Context) (domain.ProviderCapabilities, error) {
	return domain.ProviderCapabilities{}, nil
}
func (fake) Create(context.Context, CreateRequest) (Instance, error) { return Instance{}, ErrCapacity }
func (fake) Update(context.Context, string, UpdateRequest) error     { return nil }
func (fake) Destroy(context.Context, string) error                   { return nil }
func (fake) List(context.Context) ([]Instance, error)                { return nil, nil }

var _ Provider = fake{}

func TestBootstrapNeverLogged(t *testing.T) {
	const token = "fcb_supersecret"
	b := Bootstrap{ServerURL: "https://forecrew.example.com", Token: token}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("boot", "bootstrap", b, "request", CreateRequest{Bootstrap: &b}.Bootstrap)
	out := buf.String() + fmt.Sprint(b) + fmt.Sprintf("%v %s", b, b)
	if strings.Contains(out, token) {
		t.Fatalf("token in %q", out)
	}
	if !strings.Contains(out, b.ServerURL) {
		t.Fatalf("no server in %q", out)
	}
}

// The token travels in the API body and nowhere else.
func TestBootstrapInAPI(t *testing.T) {
	data, err := json.Marshal(CreateRequest{Bootstrap: &Bootstrap{ServerURL: "https://f", Token: "fcb_x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"token":"fcb_x"`) {
		t.Fatalf("body = %s", data)
	}
	data, err = json.Marshal(CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "bootstrap") || strings.Contains(string(data), "deadline") {
		t.Fatalf("started machine = %s", data)
	}
}
