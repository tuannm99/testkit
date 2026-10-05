Task: draft ONE TestKit test case (YAML) for the requirement below.

Write the case with the vocabulary listed below only: step names, given keys and check expressions that
exist in the vocabulary and in the service descriptor (entities, mocks, triggers, failpoints). Follow the
style of the example cases.

Requirements for the draft:
- id: use exactly the id given below; status: draft; do not add an `admission` block.
- purpose and preconditions in Vietnamese; every expectation has an `id` (A1, A2, …), a check, an
  operator with an expected value, and a `why` in Vietnamese.
- Test the requirement's behaviour end to end through the declared triggers; expectations must be able
  to fail when the behaviour is wrong (no tautologies, no checks that always hold).
- Add at least one entry under `mutations` using a failpoint of the service that breaks the behaviour
  under test, with `expect_red` naming the expectations that must turn red. If no failpoint fits, add a
  YAML comment at the top saying which failpoint would be needed.
- Use per-run data only: identifiers and addresses that embed {{ .ns }} where the examples do.
- No secrets, no real personal data, no real third-party URLs, no sleeps.

Answer with the YAML only, inside one ```yaml fence.
