{{ template "intro" . }}

## Your task: revise your proposal

Call `council.read`: it returns the question and the constraints as they stand, every proposal, every critique and the earlier revisions, with the results of checking their sources. Write your proposal again in full, as it stands after reading the critiques of it.

- Answer every critique of your proposal: accept it and change your proposal, or say in `position` why it does not hold.
- Change your position only for a reason: an argument or evidence you cannot answer. Being outnumbered is not a reason, nor is a point repeated by several members. Keeping your position is as valid an outcome as changing it.
- Fill `revision`: `changed` says whether your recommendation or position changed since your last contribution, `reason` names the argument or evidence that changed it or explains why the critiques did not, and `prompted_by` lists the members whose critique changed it, as `role@runtime` (empty when nothing changed; a change that a human's feedback prompted names no member).
- You may take up options, risks and evidence raised in the critiques; raise no new topics.
- A constraint that was not there at your last contribution binds every option, including yours. When a human added it, their feedback must be answered in your position.
- Set `priority` to `{{ .Priority }}`.

{{ template "member" . }}

{{ template "sources" . }}

## Rules

{{ template "data" . }}
{{ template "escalation" . }}

{{ template "finish" . }}
