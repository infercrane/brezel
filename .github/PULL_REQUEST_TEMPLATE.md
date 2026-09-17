## Outcome

Describe the user-visible or operational result.

## Boundary

- [ ] No trust, lifecycle, API, or storage boundary changes
- [ ] Boundary changes are covered by the threat model and an ADR
- [ ] Claims distinguish implemented, qualified, and planned behavior

## Verification

- [ ] Regression tests cover behavior changes
- [ ] `make check` passes
- [ ] No credentials, customer content, or capability URLs appear in logs or fixtures
- [ ] Linux/KVM qualification evidence is attached when the microVM boundary changed

List the exact checks and deployment profile used:
