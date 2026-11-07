package tart

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/fleet"
)

// guestDir is where forecrew-start looks for its settings
// (internal/images/guest/com.forecrew.worker.plist): a folder of mode 0700 root
// on the machine's own disk.
const guestDir = "/var/run/forecrew"

// deliverScript is what runs in the guest, as root. The settings arrive on
// standard input, never in an argument; they are written aside and renamed, so
// that launchd, which starts the worker when the file appears, never sees half
// of it. $1 is the folder.
const deliverScript = `set -eu
umask 077
d=$1
mkdir -p "$d"
chmod 700 "$d"
tmp=$d/.bootstrap.env.tmp
trap 'rm -f "$tmp"' EXIT
cat >"$tmp"
mv "$tmp" "$d/bootstrap.env"`

// settingValue is what forecrew-start accepts in a value: it refuses the rest.
var settingValue = regexp.MustCompile(`^[A-Za-z0-9._/:@+,-]+$`)

// bootstrapEnv writes the settings file of the worker.
func bootstrapEnv(b *fleet.Bootstrap) (string, error) {
	if !settingValue.MatchString(b.ServerURL) {
		return "", errors.New("the server URL has characters a machine's start script does not take (letters, digits and . _ / : @ + , -)")
	}
	if !settingValue.MatchString(b.Token) {
		return "", errors.New("the bootstrap token has characters a machine's start script does not take")
	}
	return "FORECREW_SERVER_URL=" + b.ServerURL + "\nFORECREW_BOOTSTRAP_TOKEN=" + b.Token + "\n", nil
}

// deliver puts the settings in the running guest, through its guest agent.
func (p *Provider) deliver(ctx context.Context, name, settings string) error {
	err := p.cli.exec(ctx, settings, name, "sudo", "-n", "/bin/sh", "-c", deliverScript, "sh", guestDir)
	if err != nil {
		return fmt.Errorf("deliver the bootstrap settings: %w", err)
	}
	return nil
}

// waitGuest waits until the guest agent answers, which is when the machine can
// take its settings. exited reports that the VM's process ended.
func (p *Provider) waitGuest(ctx context.Context, id string, exited <-chan error) error {
	name := vmName(id)
	limit := time.NewTimer(time.Duration(p.cfg.BootWaitSeconds) * time.Second)
	defer limit.Stop()
	var last error
	for {
		if last = p.cli.exec(ctx, "", name, "/usr/bin/true"); last == nil {
			return nil
		}
		wait := time.NewTimer(p.poll)
		select {
		case err := <-exited:
			wait.Stop()
			if strings.Contains(strings.ToLower(tailRaw(p.logPath(id))), "limitexceeded") {
				// Apple's limit, reached by VMs this agent cannot see: no room, not a failure.
				return fmt.Errorf("%w: tart run says the Mac's limit of VMs is reached", fleet.ErrCapacity)
			}
			return fmt.Errorf("the VM ended before its guest agent answered (%v): tart said %s", err, tail(p.logPath(id)))
		case <-limit.C:
			wait.Stop()
			return fmt.Errorf("the guest agent did not answer within %d s: %w", p.cfg.BootWaitSeconds, last)
		case <-ctx.Done():
			wait.Stop()
			return ctx.Err()
		case <-wait.C:
		}
	}
}

// hostList joins values for a flag.
func hostList(items []string) string { return strings.Join(items, ",") }
