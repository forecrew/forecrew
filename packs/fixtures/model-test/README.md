# Suitability test fixture

The work tree of a model's suitability test (docs/contracts/config-resources.md,
Model checks). The worker copies it, writes a code word into `notes.txt` and a
token into `token.txt`, runs the agent on it, and then looks at `greeting.txt`
and `.check-ran`. It never holds a project's code.
