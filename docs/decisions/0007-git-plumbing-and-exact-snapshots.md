# 7. Git plumbing and exact snapshots

Decision: use the Git command line with fixed arguments and isolated
configuration, and build validation inputs from raw objects listed with
NUL delimiters and read by size, never from an archive export.

Why: export-ignore attributes can drop tracked files from an archive, and
attribute filters can transform content. Materializing from blobs with
explicit modes and a verification pass reproduces exactly the tree the
proposal will contain, and the proposal commit is built from a persisted
recipe so its id is deterministic and independent of the checkout's HEAD.

Alternatives: go-git; `git archive`; validating the working tree.

Tradeoffs: a Git binary on the host; symlinks and submodules are refused
rather than reinterpreted.

Revisit when: a supported repository needs tracked symlinks.
