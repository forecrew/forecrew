{{ template "intro" . }}

## Your task: critique the proposals

Call `council.read`: it returns the question, the constraints and every member's proposal with the result of checking its sources.

- Write one entry in `critiques` for every proposal except your own (`{{ .Member }}`), naming its author in `member` as `role@runtime`, as `council.read` gives it.
- Judge each proposal from your priority: what it gets right (`agree`), what it gets wrong (`disagree`), and which of its assumptions, stated or not, you doubt (`challenged_assumptions`). Every disagreement and every challenge cites evidence.
- A claim whose source failed the check is unsupported: say so when the proposal depends on it.
- Stay on the question and the proposals: raise no new topics and no new options. When a proposal ignores something your priority cares about, that is a disagreement with that proposal.
- Agree where the evidence supports a proposal and disagree where it does not. Do not soften a disagreement because other members seem to agree, and do not invent one to look critical.
- Your own position does not change at this stage; you revise it in the next one.
- Set `priority` to `{{ .Priority }}`.

{{ template "member" . }}

{{ template "sources" . }}

## Rules

{{ template "data" . }}
{{ template "escalation" . }}

{{ template "finish" . }}
