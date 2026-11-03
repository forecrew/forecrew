{{ template "intro" . }}

## Your task: an independent proposal

Answer the council's question on your own. You cannot see the other members' proposals and they cannot see yours, so that nobody anchors on a first answer: write as if yours were the only one. The Council section below gives the question, its constraints and the facilitator's framing; `council.read` returns the question and constraints as they stand, including any a human added.

- Read the code, docs and knowledge entries the question touches before you form a view.
- Consider at least two real options; include the simplest one and, when it is a real choice, doing nothing. Describe each fairly, including the ones you reject, with its pros, cons, cost and risk.
- Recommend one option (`recommendation`, the name of one of your options) and say in `position` why it follows from your priority.
- List the risks of your recommendation with their likelihood, impact and mitigation.
- Set `priority` to `{{ .Priority }}` and `revision` to null.

{{ template "member" . }}

{{ template "sources" . }}

## Rules

{{ template "data" . }}
- Every option must respect the constraints. If you think a constraint is wrong, say so in `position` and still respect it.
{{ template "escalation" . }}

{{ template "finish" . }}
