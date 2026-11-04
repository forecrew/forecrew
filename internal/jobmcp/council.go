package jobmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// councilPlaybook is the playbook of council jobs, councils.Playbook. The
// gateway knows nothing else of councils: their packs link this package's
// role tools in their tests, so it does not import them.
const councilPlaybook = "council"

// councilTurn is where a job stands in its council.
type councilTurn struct {
	council uuid.UUID
	stage   domain.CouncilStage
	round   int
}

// errNoCouncil answers a job that is not a turn of a council.
var errNoCouncil = Errorf(CodeNotFound, "this job is not part of a council")

// turnOf finds the turn of a running job. A job's ID is its step run's and
// the job has no field for its council, so the answer comes from the run: its
// input names the council, and the step's path in packs/playbooks/council.yaml
// the stage and round. A frame job reads like a proposal and a check like the
// revision round it decides, which see what the prompts of those steps need.
func turnOf(ctx context.Context, c *Call) (councilTurn, error) {
	st, acc := c.Config.Store, c.Job.AccountID
	steps, err := st.StepRuns().ListWaiting(ctx, acc, domain.JobKey(c.Job.ID))
	if err != nil {
		return councilTurn{}, err
	}
	if len(steps) == 0 {
		return councilTurn{}, errNoCouncil
	}
	run, err := st.WorkflowRuns().Get(ctx, acc, steps[0].RunID)
	if err != nil {
		return councilTurn{}, err
	}
	var inputs struct {
		CouncilID uuid.UUID `json:"council_id"`
	}
	if run.Playbook != councilPlaybook || json.Unmarshal(run.Inputs, &inputs) != nil {
		return councilTurn{}, errNoCouncil
	}
	t := councilTurn{council: inputs.CouncilID, round: 1}
	path := strings.Split(steps[0].StepID, "/")
	switch path[len(path)-1] {
	case "frame", "proposal":
		t.stage = domain.StageProposal
	case "critique":
		t.stage = domain.StageCritique
	case "check", "revision": // debate/<round>/...
		t.stage = domain.StageRevision
		t.round, err = strconv.Atoi(path[1])
	case "synthesis": // decision/<round>/...
		t.stage = domain.StageSynthesis
		t.round, err = strconv.Atoi(path[1])
	case "human_revision": // decision/<brief>/...: the round is decide's
		t.stage = domain.StageRevision
		t.round, err = humanRound(ctx, c, run.ID, path[1])
	default:
		return councilTurn{}, errNoCouncil
	}
	if err != nil {
		return councilTurn{}, fmt.Errorf("step %s: %w", steps[0].StepID, err)
	}
	return t, nil
}

// humanRound reads the human's round from the output of the decide step that
// started it.
func humanRound(ctx context.Context, c *Call, run uuid.UUID, brief string) (int, error) {
	steps, err := c.Config.Store.StepRuns().ListByRun(ctx, c.Job.AccountID, run)
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(steps, func(s domain.StepRun) bool { return s.StepID == "decision/"+brief+"/decide" })
	if i < 0 {
		return 0, errors.New("no decide step")
	}
	var out struct {
		Output struct {
			Round int `json:"round"`
		} `json:"output"`
	}
	err = json.Unmarshal(steps[i].Output, &out)
	return out.Output.Round, err
}

// sees reports whether a job of turn t may read k.
func (t councilTurn) sees(k domain.CouncilContribution) bool {
	switch t.stage {
	case domain.StageCritique:
		return k.Stage == domain.StageProposal
	case domain.StageRevision:
		return k.Stage == domain.StageProposal || k.Stage == domain.StageCritique || (k.Stage == domain.StageRevision && k.Round < t.round)
	case domain.StageSynthesis:
		return true
	}
	return false
}

// councilContribution is an item of council_read's contributions.
type councilContribution struct {
	Member        string               `json:"member"` // role@runtime
	Runtime       string               `json:"runtime"`
	Stage         domain.CouncilStage  `json:"stage"`
	Round         int                  `json:"round"`
	Content       json.RawMessage      `json:"content"`
	EvidenceCheck []domain.SourceCheck `json:"evidence_check"`
}

type councilReadOutput struct {
	Council       string                `json:"council"`
	Question      string                `json:"question"`
	Constraints   []string              `json:"constraints"`
	Stage         domain.CouncilStage   `json:"stage"`
	Round         int                   `json:"round"`
	Contributions []councilContribution `json:"contributions"`
}

// councilRead is the tool council_read. Contributions are other agents'
// output: the text content holds each between untrusted-data markers.
func councilRead(ctx context.Context, c *Call, _ json.RawMessage) (Result, error) {
	t, err := turnOf(ctx, c)
	if err != nil {
		return Result{}, err
	}
	st, acc := c.Config.Store, c.Job.AccountID
	council, err := st.Councils().Get(ctx, acc, t.council)
	if errors.Is(err, store.ErrNotFound) {
		return Result{}, errNoCouncil
	}
	if err != nil {
		return Result{}, err
	}
	all, err := st.Councils().Contributions(ctx, acc, council.ID, "")
	if err != nil {
		return Result{}, err
	}
	out := councilReadOutput{
		Council: council.Ref(), Question: council.Question, Constraints: council.Constraints, Stage: t.stage, Round: t.round,
		Contributions: []councilContribution{},
	}
	head, err := json.Marshal(out)
	if err != nil {
		return Result{}, err
	}
	var text strings.Builder
	text.Write(head)
	text.WriteString("\n")
	for _, k := range all {
		if !t.sees(k) {
			continue
		}
		item := councilContribution{
			Member: k.Role + "@" + k.Runtime, Runtime: k.Runtime, Stage: k.Stage, Round: k.Round, Content: k.Content, EvidenceCheck: k.EvidenceCheck,
		}
		out.Contributions = append(out.Contributions, item)
		raw, err := json.Marshal(item)
		if err != nil {
			return Result{}, err
		}
		text.WriteString(contextpack.WrapUntrusted(fmt.Sprintf("%s, %s round %d", item.Member, k.Stage, k.Round), string(raw)))
	}
	return Result{Data: out, Text: text.String()}, nil
}
