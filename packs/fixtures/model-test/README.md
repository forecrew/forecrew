# Suitability test fixture

The work tree of a model's suitability test (docs/contracts/config-resources.md,
Model checks). The worker copies it, writes a code word into `notes.txt` and a
different random token into `token.txt`, runs the agent on it with a per-run value in
`FORECREW_CHECK_SALT`, and then looks at `greeting.txt` and `.check-ran`. A
right `.check-ran` shows the agent used a shell that knew that value, as
running `check.sh` does; it does not prove `check.sh` itself ran. It never holds a project's code.
