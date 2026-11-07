package aws

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/forecrew/forecrew/internal/fleet"
)

// consistencyWindow is how long an instance this process started is listed even
// when EC2 does not show it yet: DescribeInstances is eventually consistent, and
// a machine missing from a list is failed as gone.
const consistencyWindow = 2 * time.Minute

// seen is an instance the provider saw, and when.
type seen struct {
	machine string
	inst    fleet.Instance
	at      time.Time
}

// liveStates are the states of an instance that exists and costs money.
var liveStates = []string{"pending", "running", "stopping", "stopped"}

func tagFilter(key string, values ...string) types.Filter {
	return types.Filter{Name: awssdk.String("tag:" + key), Values: values}
}

// ours reports that an instance carries this agent's fleet tag: the only kind
// the provider lists, changes or terminates, whatever EC2 returned.
func (p *Provider) ours(i types.Instance) bool {
	for _, t := range i.Tags {
		if awssdk.ToString(t.Key) == TagFleet {
			return awssdk.ToString(t.Value) == p.cfg.Fleet
		}
	}
	return false
}

func tagOf(i types.Instance, key string) string {
	for _, t := range i.Tags {
		if awssdk.ToString(t.Key) == key {
			return awssdk.ToString(t.Value)
		}
	}
	return ""
}

// describe returns the instances in states that match the filters and ids, ours
// only.
func (p *Provider) describe(ctx context.Context, ids []string, filters ...types.Filter) ([]types.Instance, error) {
	in := &ec2.DescribeInstancesInput{InstanceIds: ids, Filters: filters}
	var out []types.Instance
	for pages := ec2.NewDescribeInstancesPaginator(p.ec2, in); pages.HasMorePages(); {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				if p.ours(i) {
					out = append(out, i)
				}
			}
		}
	}
	return out, nil
}

// live reports an instance that exists and costs money: the filters ask for
// these states, and this is the check on what came back.
func live(i types.Instance) bool {
	return i.State != nil && slices.Contains(liveStates, string(i.State.Name))
}

// listOurs returns every instance of this agent's fleet that exists.
func (p *Provider) listOurs(ctx context.Context) ([]types.Instance, error) {
	all, err := p.describe(ctx, nil, tagFilter(TagFleet, p.cfg.Fleet), types.Filter{Name: awssdk.String("instance-state-name"), Values: liveStates})
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return slices.DeleteFunc(all, func(i types.Instance) bool { return !live(i) }), nil
}

// destroyMachine terminates what exists for a machine ID. The caller holds launch
// or does not care about a race.
func (p *Provider) destroyMachine(ctx context.Context, machineID string) error {
	found, err := p.describe(ctx, nil, tagFilter(TagFleet, p.cfg.Fleet), tagFilter(fleet.TagMachine, machineID),
		types.Filter{Name: awssdk.String("instance-state-name"), Values: liveStates})
	if err != nil {
		return fmt.Errorf("find the machine: %w", err)
	}
	for _, i := range found {
		// The filter did the choosing; this is the check that nothing else is ended.
		if tagOf(i, fleet.TagMachine) != machineID || !live(i) {
			continue
		}
		if err := p.terminate(ctx, awssdk.ToString(i.InstanceId)); err != nil {
			return err
		}
	}
	return nil
}

// terminate ends an instance the caller has checked is ours. EC2 refuses it for
// anything else, through the role's policy. An instance that is gone is ended.
func (p *Provider) terminate(ctx context.Context, id string) error {
	_, err := p.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err != nil && !hasCode(err, notFoundCodes) {
		return fmt.Errorf("terminate %s: %w", id, err)
	}
	p.forget(id)
	if p.ledger != nil {
		if err := p.ledger.end(id); err != nil {
			p.log.Warn("spend ledger", "instance", id, "error", err)
		}
	}
	return nil
}

func (p *Provider) forget(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.timers[id]; t != nil {
		t.Stop()
		delete(p.timers, id)
	}
	delete(p.known, id)
}

// arm ends instance id at deadline, whatever forecrewd says or can say. It is the
// second line: the first is the machine's own timer, and a restarted agent arms
// again from the tags at its first List.
func (p *Provider) arm(id string, deadline time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.timers[id]; t != nil {
		t.Stop()
		delete(p.timers, id)
	}
	if deadline.IsZero() {
		return
	}
	p.timers[id] = time.AfterFunc(max(deadline.Sub(p.now()), 0), func() {
		p.log.Info("machine past its deadline: terminating it", "instance", id)
		ctx, cancel := context.WithTimeout(context.Background(), terminateTimeout)
		defer cancel()
		if err := p.terminate(ctx, id); err != nil {
			p.log.Warn("terminate a machine past its deadline", "instance", id, "error", err)
		}
	})
}

// Destroy implements fleet.Provider. It terminates the instance, which takes its
// volume and network interface (they are made to be deleted with it). An instance
// that does not exist, or is already ending, succeeds; one that is not this
// agent's is refused.
func (p *Provider) Destroy(ctx context.Context, externalID string) error {
	if !instanceRE.MatchString(externalID) {
		return fmt.Errorf("%q is not an instance ID", externalID)
	}
	out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{externalID}})
	if hasCode(err, notFoundCodes) {
		p.forget(externalID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("describe %s: %w", externalID, err)
	}
	i, found := firstInstance(out)
	switch {
	case !found:
		p.forget(externalID)
		return nil
	case i.State != nil && (i.State.Name == types.InstanceStateNameTerminated || i.State.Name == types.InstanceStateNameShuttingDown):
		p.forget(externalID)
		return p.endLedger(externalID)
	case !p.ours(i):
		return fmt.Errorf("instance %s is not a machine of this agent (fleet %s)", externalID, p.cfg.Fleet)
	}
	return p.terminate(ctx, externalID)
}

func firstInstance(out *ec2.DescribeInstancesOutput) (types.Instance, bool) {
	for _, r := range out.Reservations {
		if len(r.Instances) > 0 {
			return r.Instances[0], true
		}
	}
	return types.Instance{}, false
}

func (p *Provider) endLedger(id string) error {
	if p.ledger == nil {
		return nil
	}
	return p.ledger.end(id)
}

// Update implements fleet.Provider: the tags and the deadline are each the whole
// new value. Tags of ours that nothing asks for any more are removed; the fleet
// and machine tags stay, and the role's policy refuses to change them.
func (p *Provider) Update(ctx context.Context, externalID string, req fleet.UpdateRequest) error {
	if !instanceRE.MatchString(externalID) {
		return fmt.Errorf("%q is not an instance ID", externalID)
	}
	if len(req.NetworkAllow) > 0 {
		return fmt.Errorf("network_allow does not apply to an AWS location: what a machine reaches is set by its security group")
	}
	found, err := p.describe(ctx, []string{externalID})
	if err != nil {
		return fmt.Errorf("describe %s: %w", externalID, err)
	}
	if len(found) == 0 || found[0].State == nil || slices.Contains([]types.InstanceStateName{types.InstanceStateNameTerminated, types.InstanceStateNameShuttingDown}, found[0].State.Name) {
		return fmt.Errorf("no machine %q", externalID)
	}
	set, drop := tagChanges(found[0], req)
	if len(set) > 0 {
		tags, err := tagList(set)
		if err != nil {
			return err
		}
		if _, err := p.ec2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{externalID}, Tags: tags}); err != nil {
			return fmt.Errorf("tag %s: %w", externalID, err)
		}
	}
	if len(drop) > 0 {
		if _, err := p.ec2.DeleteTags(ctx, &ec2.DeleteTagsInput{Resources: []string{externalID}, Tags: drop}); err != nil {
			return fmt.Errorf("untag %s: %w", externalID, err)
		}
	}
	p.arm(externalID, req.Deadline)
	return nil
}

// protectedTag is a tag that a request cannot set or remove: ours, or EC2's.
func protectedTag(k string) bool {
	return k == "Name" || strings.HasPrefix(k, "aws:") || k == fleet.TagMachine || k == TagFleet || k == TagPrice
}

// tagChanges is what Update must set on, and remove from, an instance to leave
// it with the tags and deadline the request says.
func tagChanges(inst types.Instance, req fleet.UpdateRequest) (set map[string]string, drop []types.Tag) {
	want := maps.Clone(req.Tags)
	if want == nil {
		want = map[string]string{}
	}
	if !req.Deadline.IsZero() {
		want[TagDeadline] = req.Deadline.UTC().Format(time.RFC3339)
	}
	for k := range want {
		if protectedTag(k) {
			delete(want, k) // not for a request to change
		}
	}
	have := map[string]string{}
	for _, t := range inst.Tags {
		if k := awssdk.ToString(t.Key); !protectedTag(k) {
			have[k] = awssdk.ToString(t.Value)
			if _, ok := want[k]; !ok {
				drop = append(drop, types.Tag{Key: awssdk.String(k)})
			}
		}
	}
	set = map[string]string{}
	for k, v := range want {
		if have[k] != v {
			set[k] = v
		}
	}
	return set, drop
}

// List implements fleet.Provider. It is also the cleaner: what outlived its
// deadline or its maximum life, or is stopped (a machine never is), is terminated
// here, so an agent that is the only thing left still ends what leaked, and what
// is gone is not listed, which is how forecrewd learns a machine was lost.
func (p *Provider) List(ctx context.Context) ([]fleet.Instance, error) {
	all, err := p.listOurs(ctx)
	if err != nil {
		return nil, err
	}
	now := p.now()
	var out []fleet.Instance
	live := map[string]bool{}
	for _, i := range all {
		id := awssdk.ToString(i.InstanceId)
		if why := p.stale(i, now); why != "" {
			p.log.Warn("terminating a machine that should not exist", "instance", id, "why", why)
			if err := p.terminate(ctx, id); err == nil {
				continue
			} else {
				p.log.Warn("terminate a stale machine", "instance", id, "error", err)
			}
		}
		// A deadline already past is the next List's to enforce, after its grace.
		if d := deadlineOf(i); d.After(now) {
			p.arm(id, d)
		}
		live[id] = true
		out = append(out, p.instance(i))
	}
	out = append(out, p.recent(live, now)...)
	p.lost(ctx, live)
	if p.ledger != nil {
		p.meter(all, live)
	}
	slices.SortFunc(out, func(a, b fleet.Instance) int { return strings.Compare(a.ExternalID, b.ExternalID) })
	return out, nil
}

func deadlineOf(i types.Instance) time.Time {
	t, err := time.Parse(time.RFC3339, tagOf(i, TagDeadline))
	if err != nil {
		return time.Time{}
	}
	return t
}

// stale says why an instance should not exist any more, or "".
func (p *Provider) stale(i types.Instance, now time.Time) string {
	switch {
	case i.State != nil && (i.State.Name == types.InstanceStateNameStopped || i.State.Name == types.InstanceStateNameStopping):
		return "it is stopped"
	case i.LaunchTime != nil && now.Sub(*i.LaunchTime) > p.cfg.MaxLifetime:
		return "it outlived max_lifetime"
	}
	if d := deadlineOf(i); !d.IsZero() && now.After(d.Add(sweepGrace)) {
		return "it is past its deadline"
	}
	return ""
}

// recent returns the instances this process started a moment ago that EC2 does
// not list yet.
func (p *Provider) recent(live map[string]bool, now time.Time) []fleet.Instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []fleet.Instance
	for id, s := range p.known {
		if !live[id] && s.recent(now) {
			live[id] = true
			out = append(out, s.inst)
		}
	}
	return out
}

// recent reports an instance this process started within consistencyWindow.
func (s seen) recent(now time.Time) bool {
	return !s.at.IsZero() && now.Sub(s.at) < consistencyWindow
}

// pending counts the instances this process started a moment ago that live does
// not show: EC2 lags, and two Creates must not both take the last place.
func (p *Provider) pending(live []types.Instance, now time.Time) int {
	shown := map[string]bool{}
	for _, i := range live {
		shown[awssdk.ToString(i.InstanceId)] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for id, s := range p.known {
		if !shown[id] && s.recent(now) {
			n++
		}
	}
	return n
}

// lost says in the log why instances this process knew are gone: a spot machine
// taken back is the case to look for. forecrewd fails such a machine at its next
// reconcile and its job is requeued through its lease.
func (p *Provider) lost(ctx context.Context, live map[string]bool) {
	p.mu.Lock()
	var gone []string
	for id := range p.known {
		if !live[id] {
			gone = append(gone, id)
		}
	}
	for id := range live {
		if _, ok := p.known[id]; !ok {
			p.known[id] = seen{}
		}
	}
	p.mu.Unlock()
	for _, id := range gone {
		p.forget(id)
		p.log.Warn("a machine is gone from EC2", "instance", id, "reason", p.whyGone(ctx, id))
	}
}

// whyGone asks EC2 for the reason an instance ended, which it keeps for an hour.
func (p *Provider) whyGone(ctx context.Context, id string) string {
	out, err := p.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return "unknown"
	}
	if i, ok := firstInstance(out); ok && i.StateReason != nil {
		return awssdk.ToString(i.StateReason.Code)
	}
	return "unknown"
}

// meter makes the ledger agree with what exists: a machine that is gone stops
// costing, and one the ledger never heard of (a lost file) counts from its launch.
func (p *Provider) meter(all []types.Instance, live map[string]bool) {
	if err := p.ledger.onlyOpen(live); err != nil {
		p.log.Warn("spend ledger", "error", err)
	}
	for _, i := range all {
		id := awssdk.ToString(i.InstanceId)
		price, err := parsePrice(tagOf(i, TagPrice))
		if err != nil {
			continue
		}
		if err := p.ledger.adopt(id, awssdk.ToTime(i.LaunchTime), price); err != nil {
			p.log.Warn("spend ledger", "error", err)
		}
	}
}
