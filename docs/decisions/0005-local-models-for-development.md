# 5. Local models during development; paid calls only as a measurement

Decision: develop and test the model-driven agent against local Ollama
models and recorded responses. No development or test path calls a paid
provider.

Why: live paid calls are unnecessary to build the mechanics, make tests
slow and non-reproducible, and risk uncontrolled spend or a key in the
wrong place. A local model gives a real tool-calling loop for free, and
recordings make model mode replayable in CI with the server unreachable.

Alternatives: a paid provider from the start, behind caps.

Tradeoffs: local model quality is lower, which the benchmarks show
honestly. A paid provider is still the right instrument for one explicit,
budgeted measurement, never for the workflow.

Revisit when: that measurement is wanted; it needs an opt-in flag, a key
in the environment, and the runtime's cost cap.
