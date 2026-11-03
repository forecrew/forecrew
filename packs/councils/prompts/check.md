{{ template "intro" . }}

## Your task: do positions still move?

Call `council.read`: it returns every contribution so far. Decide whether another revision round could change a member's position. Round {{ .Round }} of this check decides whether revision round {{ .Round }} runs.

- `positions_moving` is true when a critique a member has not answered could change their position, when new evidence was raised that a member has not weighed, or when a member changed position in the last round and others have not responded. It is false when every member has answered the critiques of their proposal, agreed or not.
- Do not judge which position is right, and do not try to reach agreement: a disagreement that has settled goes to the synthesis as dissent, and another round will not resolve it.
- `reason` names the members and the points you based the judgement on; `open_disagreements` lists the disagreements still standing, with the members on each side.

## Rules

{{ template "data" . }}

{{ template "finish" . }}
