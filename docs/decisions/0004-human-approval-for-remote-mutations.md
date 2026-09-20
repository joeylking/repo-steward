# 4. Remote mutations require a human approval bound to the request

Decision: nothing leaves the machine without an approval whose hash covers
the kind, capability, presentation, and the exact request; publication
approvals name the frozen proposal's id and hash.

Why: the model reads untrusted content and can be wrong or manipulated.
Local edits are cheap to review and revert; a push or a pull request is
visible to other people. Binding the approval to a hash means a changed
proposal cannot inherit an earlier decision, and the runtime executes only
the recorded request.

Alternatives: auto-publishing when validation passes; approval by chat
message.

Tradeoffs: a paused run needs a second command to continue, and approvals
can expire.

Revisit when: never for publication. Scope expansion is the only other
approval and is granted at most once per run.
