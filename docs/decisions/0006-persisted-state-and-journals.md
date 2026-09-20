# 6. Persisted state and journals, not event replay

Decision: SQLite tables are the state; events are an audit log. Manifest
promotions, proposal preparation, and publication are journaled with
enough identity to finish, restore, or stop after an interruption.

Why: recovery must not depend on artifacts that may legitimately be gone,
nor infer success from an artifact's existence. Journals record intent
and before and after identities; recovery compares them with the
workspace and the remote. Phase is derived from the journals, so there is
no cross-store consistency hazard.

Alternatives: event sourcing; inferring state from the filesystem.

Tradeoffs: more explicit rows; fault-injected tests at every journaled
point keep them honest.

Revisit when: a consumer needs replay into alternative states.
