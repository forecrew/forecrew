package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

const (
	// busyShare is how full a machine's vCPUs are, on average between two
	// looks, to count as busy.
	busyShare = 0.9
	// userHZ is the unit of the CPU times of /proc: 100 on every Linux the
	// fleet runs on.
	userHZ = 100
)

var _ fleet.AbuseChecker = (*Provider)(nil)

// AbuseConfig is microvm.yaml abuse; a zero field turns its check off.
type AbuseConfig struct {
	// CPUMinutes stops a machine whose vCPUs stay all but full this long.
	CPUMinutes int `yaml:"cpu_minutes"`
	// MiningMinutes stops a machine whose vCPUs stay full this long while it
	// opens connections to the ports of mining pools (miningPorts).
	MiningMinutes int `yaml:"mining_minutes"`
}

// cpuWatch is what the checks last saw of a machine.
type cpuWatch struct {
	at        time.Time
	used      time.Duration
	busySince time.Time // zero when it was not busy at the last look
}

// CheckAbuse implements fleet.AbuseChecker: it looks at each running machine's
// CPU time and its mining counter, and stops at once, by killing its VMM, a
// machine that crosses a limit of abuse. Its cache is not kept. The agent
// reports it; forecrewd cancels its job and destroys the rest.
func (p *Provider) CheckAbuse(ctx context.Context) ([]fleet.AbuseReport, error) {
	if p.cfg.Abuse.CPUMinutes <= 0 && p.cfg.Abuse.MiningMinutes <= 0 {
		return nil, nil
	}
	all, err := p.records()
	if err != nil {
		return nil, err
	}
	now := p.now()
	p.watchMu.Lock()
	defer p.watchMu.Unlock()
	var out []fleet.AbuseReport
	var errs []error
	seen := map[string]bool{}
	for _, r := range all {
		if r.Abuse != "" || !running(r) {
			continue
		}
		seen[r.ID] = true
		busy, err := p.busyFor(r, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("machine %s: %w", r.ID, err))
			continue
		}
		kind, detail := p.judge(ctx, r, busy)
		if kind == "" {
			continue
		}
		if err := p.stop(r, kind); err != nil {
			errs = append(errs, err)
			continue
		}
		report := fleet.AbuseReport{ExternalID: r.ID, Kind: kind, Detail: detail}
		if id, err := parseID(r.ID); err == nil {
			report.MachineID = id
		}
		out = append(out, report)
	}
	for id := range p.watch {
		if !seen[id] {
			delete(p.watch, id)
		}
	}
	return out, errors.Join(errs...)
}

// busyFor says how long r's vCPUs have been busy without a pause, from what
// they used since the last look. The first look says nothing. The caller
// holds watchMu.
func (p *Provider) busyFor(r *record, now time.Time) (time.Duration, error) {
	used, err := p.cpuTime(r.PID)
	if err != nil {
		return 0, err
	}
	w := p.watch[r.ID]
	if w == nil {
		p.watch[r.ID] = &cpuWatch{at: now, used: used}
		return 0, nil
	}
	elapsed := now.Sub(w.at)
	if elapsed <= 0 {
		return 0, nil
	}
	share := (used - w.used).Seconds() / elapsed.Seconds() / float64(max(r.CPU, 1))
	switch {
	case share < busyShare:
		w.busySince = time.Time{}
	case w.busySince.IsZero():
		w.busySince = w.at
	}
	w.at, w.used = now, used
	if w.busySince.IsZero() {
		return 0, nil
	}
	return now.Sub(w.busySince), nil
}

// judge says whether r crossed a limit, and in words what it did.
func (p *Provider) judge(ctx context.Context, r *record, busy time.Duration) (kind, detail string) {
	minutes := int(busy.Minutes())
	if n := p.cfg.Abuse.MiningMinutes; n > 0 && busy >= time.Duration(n)*time.Minute {
		if c, err := p.miningConnections(ctx, r); err == nil && c > 0 {
			return domain.AbuseMining, fmt.Sprintf("its vCPUs were full for %d minutes while it opened %d connections to ports of mining pools", minutes, c)
		}
	}
	if n := p.cfg.Abuse.CPUMinutes; n > 0 && busy >= time.Duration(n)*time.Minute {
		return domain.AbuseCPU, fmt.Sprintf("its vCPUs were full for %d minutes, the limit of this location", minutes)
	}
	return "", ""
}

// stop marks r as stopped for kind, so its cache is not kept, and kills its
// VMM.
func (p *Provider) stop(r *record, kind string) error {
	r.Abuse = kind
	p.mu.Lock()
	err := p.save(r)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	p.log.Warn("machine stopped for abuse: killing it", "machine_id", r.ID, "kind", kind)
	return kill(r)
}

// miningConnections reads how many new connections r opened to mining ports.
func (p *Provider) miningConnections(ctx context.Context, r *record) (int64, error) {
	out, err := p.output(ctx, "nft", "-j", "list", "counter", "inet", networkOf(p.subnet, r.Slot).Table, "mining")
	if err != nil {
		return 0, err
	}
	var v struct {
		Nftables []struct {
			Counter *struct {
				Packets int64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return 0, fmt.Errorf("read the mining counter: %w", err)
	}
	for _, x := range v.Nftables {
		if x.Counter != nil {
			return x.Counter.Packets, nil
		}
	}
	return 0, errors.New("no mining counter")
}

// cpuTime is the CPU time a process used, all its threads: a VMM's vCPUs are
// its threads.
func cpuTime(pid int) (time.Duration, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")) // #nosec G304 -- a path of /proc
	if err != nil {
		return 0, err
	}
	return parseCPUTime(string(raw))
}

// parseCPUTime reads utime and stime of a /proc/<pid>/stat line; the fields
// count from the end of the name, which may hold spaces.
func parseCPUTime(stat string) (time.Duration, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, errors.New("bad stat")
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 13 {
		return 0, errors.New("bad stat")
	}
	var ticks int64
	for _, s := range f[11:13] {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad stat: %w", err)
		}
		ticks += n
	}
	return time.Duration(ticks) * time.Second / userHZ, nil
}

func outputOf(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output() // #nosec G204 -- fixed commands, with values of this package
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, args[0], err)
	}
	return out, nil
}
