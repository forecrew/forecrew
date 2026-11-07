package aws

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/fleet"
)

func TestUserData(t *testing.T) {
	enc, err := userData(&fleet.Bootstrap{ServerURL: "https://forecrew.example.com:8443", Token: testToken}, 90*time.Minute)
	must(t, err)
	raw, err := base64.StdEncoding.DecodeString(enc)
	must(t, err)
	if !strings.HasPrefix(string(raw), "#cloud-config\n") {
		t.Fatalf("not a cloud-config: %s", raw)
	}
	var doc struct {
		WriteFiles []struct {
			Path, Permissions, Owner, Content string
		} `yaml:"write_files"`
		Runcmd [][]string `yaml:"runcmd"`
	}
	must(t, yaml.Unmarshal(raw, &doc))
	if len(doc.WriteFiles) != 1 {
		t.Fatalf("files = %+v", doc.WriteFiles)
	}
	f := doc.WriteFiles[0]
	if f.Path != "/run/forecrew/bootstrap.env" || f.Permissions != "0600" || f.Owner != "root:root" {
		t.Errorf("file = %+v", f)
	}
	if f.Content != "FORECREW_SERVER_URL=https://forecrew.example.com:8443\nFORECREW_BOOTSTRAP_TOKEN="+testToken+"\n" {
		t.Errorf("content = %q", f.Content)
	}
	if len(doc.Runcmd) != 2 || strings.Join(doc.Runcmd[0], " ") != "shutdown -P +90" || doc.Runcmd[1][0] != "sh" {
		t.Errorf("runcmd = %v", doc.Runcmd)
	}
	if got, _ := userData(&fleet.Bootstrap{ServerURL: "https://x.example", Token: "fcb_x"}, time.Second); !strings.Contains(string(mustDecode(t, got)), "+1'") {
		t.Error("a lifetime under a minute still waits one")
	}
	for _, b := range []*fleet.Bootstrap{nil, {ServerURL: "https://x\nFORECREW_WORKER_TOKEN=y", Token: "a"}, {ServerURL: "https://x", Token: "a b"}, {ServerURL: "", Token: "a"}} {
		if _, err := userData(b, time.Hour); err == nil {
			t.Errorf("%+v was accepted", b)
		}
	}
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	must(t, err)
	return b
}
