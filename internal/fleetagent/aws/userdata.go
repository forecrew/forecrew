package aws

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/fleet"
)

// settingRE is what forecrew-start accepts in a value (internal/images/guest):
// the value reaches a YAML file there, so nothing else is let through here.
var settingRE = regexp.MustCompile(`^[A-Za-z0-9._/:@+,-]+$`)

// userData is the cloud-config a machine boots with. It is the only way a token
// reaches an EC2 instance, and it has two limits the machine's owner should know
// (docs/runbooks/byoc-aws.md): anyone in the account who may read instance
// attributes can read it, and cloud-init keeps a copy on the machine's own disk.
// Both hold a token that is spent in the first seconds, and the copy is removed
// as soon as the file is written (best effort).
//
// forecrew-start reads /run/forecrew/bootstrap.env, which is RAM, and deletes it,
// so the token is the first thing the worker spends, before any job exists.
//
// The shutdown line is the machine's own limit on its life, for when nothing
// else is left to end it. EC2 terminates an instance that shuts itself down
// (InstanceInitiatedShutdownBehavior).
func userData(b *fleet.Bootstrap, lifetime time.Duration) (string, error) {
	if b == nil {
		return "", errors.New("no bootstrap settings")
	}
	if !settingRE.MatchString(b.ServerURL) {
		return "", errors.New("the server URL has characters a machine cannot take: use plain letters, digits and . _ / : @ + , -")
	}
	if !settingRE.MatchString(b.Token) {
		return "", errors.New("the bootstrap token has characters a machine cannot take")
	}
	var s strings.Builder
	s.WriteString("#cloud-config\n")
	s.WriteString("write_files:\n")
	s.WriteString("  - path: /run/forecrew/bootstrap.env\n")
	s.WriteString("    permissions: '0600'\n")
	s.WriteString("    owner: root:root\n")
	s.WriteString("    content: |\n")
	fmt.Fprintf(&s, "      FORECREW_SERVER_URL=%s\n", b.ServerURL)
	fmt.Fprintf(&s, "      FORECREW_BOOTSTRAP_TOKEN=%s\n", b.Token)
	s.WriteString("runcmd:\n")
	fmt.Fprintf(&s, "  - [shutdown, -P, '+%d']\n", max(int(lifetime.Minutes()), 1))
	s.WriteString("  - [sh, -c, 'rm -f /var/lib/cloud/instance/user-data.txt* /var/lib/cloud/instance/cloud-config.txt /var/lib/cloud/instance/obj.pkl']\n")
	return base64.StdEncoding.EncodeToString([]byte(s.String())), nil
}
