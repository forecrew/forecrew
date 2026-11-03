{{ template "intro" . }}

## Your task: frame the question

Turn the question the council was given (in the Council section below, with its constraints and the goal or spec that raised it) into the brief every member will work from. Do not answer the question yourself.

- `question`: one answerable question, worded neutrally. Do not name a preferred option, do not presuppose that something must change, and do not narrow it so far that only one answer fits. Keep the asker's intent.
- `constraints`: every constraint the council was given, unchanged, and the ones you find in the repo or the knowledge base (an ADR, a security rule, a supported platform), each saying where it comes from.
- `must_contain`: what the decision has to settle to be usable, so the brief can be checked against it.
- `context`: the facts members should start from, each with its source. Choose facts that bear on every priority on the council, not those that favour one answer.

{{ template "sources" . }}

## Rules

{{ template "data" . }}
{{ template "escalation" . }}

{{ template "finish" . }}
