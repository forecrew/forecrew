package tart

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
// of it. $1 is the folder; the rest are resolvers, set for every network
// service first, because the machine's own, the Mac's gateway, is blocked.
const deliverScript = `set -eu
umask 077
d=$1
shift
if [ $# -gt 0 ] && command -v networksetup >/dev/null 2>&1; then
	networksetup -listallnetworkservices | sed '1d; s/^\*//' | while IFS= read -r svc; do
		networksetup -setdnsservers "$svc" "$@" >/dev/null 2>&1 || true
	done
fi
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
	args := []string{"sudo", "-n", "/bin/sh", "-c", deliverScript, "sh", guestDir}
	if p.cfg.Softnet {
		args = append(args, p.cfg.DNS...)
	}
	err := p.cli.exec(ctx, settings, name, args...)
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

// checkGateway makes sure the address the machine cannot reach is the Mac's
// address on the machine's network: the address the host answers on. Softnet lets
// a machine reach the gateway unless it is blocked, and the block is by the
// configured address, so a Mac whose VMs use another subnet would leave itself
// open. It runs before the settings go in.
func (p *Provider) checkGateway(ctx context.Context, name string) error {
	limit := time.NewTimer(time.Duration(p.cfg.BootWaitSeconds) * time.Second)
	defer limit.Stop()
	var ip string
	for {
		out, err := p.cli.output(ctx, "ip", name)
		if ip = strings.TrimSpace(out); err == nil && ip != "" {
			break
		}
		select {
		case <-limit.C:
			return fmt.Errorf("the machine has no address: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.poll):
		}
	}
	guest, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("tart ip said %s", snippet([]byte(ip), ""))
	}
	for _, n := range p.hostNets() {
		if n.Contains(guest) {
			if got := n.Addr().String(); got != p.cfg.SoftnetGateway {
				return fmt.Errorf("the Mac's address on the machines' network is %s, not %s: set softnet_gateway: %s in tart.yaml", got, p.cfg.SoftnetGateway, got)
			}
			return nil
		}
	}
	return fmt.Errorf("the Mac has no address on the machine's network %s, so its gateway cannot be checked", guest)
}

// localNets are the IPv4 addresses of this host's interfaces, with their
// networks.
func localNets() []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if p, err := netip.ParsePrefix(n.String()); err == nil && p.Addr().Is4() {
				out = append(out, p)
			}
		}
	}
	return out
}
