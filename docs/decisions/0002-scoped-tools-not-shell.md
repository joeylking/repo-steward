# 2. Scoped tools instead of shell access

Decision: the agent acts only through named tools with JSON schemas and
side-effect classes; there is no shell, no network tool, and no generic
process execution.

Why: policy can only bound what it can see. A shell hides the action
inside a string; a scoped tool exposes the path, the module, or the
version as an argument the policy evaluates before anything runs. Every
mutation path, including package-manager commands, then has a gate.

Alternatives: a sandboxed shell with an allowlist.

Tradeoffs: every capability is explicit work, and the agent cannot
improvise. That is the intent.

Revisit when: a task type needs an action no scoped tool can express, in
which case a new scoped tool is added, not a shell.
