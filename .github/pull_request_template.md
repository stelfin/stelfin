## What changed and why

<!-- What does this do, and what problem does it solve? Link the issue: "Closes #123". -->

## How it was checked

<!-- Commands you ran and what you saw. Tests added or changed. -->

## Checklist

- [ ] `make check` passes locally.
- [ ] New or changed behaviour has tests, including edge cases and not only the happy path.
- [ ] If it touches `contracts/`: `make test-contracts` passes, and if the WASM changed, the hash in `contracts/deployments.json` and the README table match a fresh `make build-contracts`.
- [ ] If it touches `marketing/`: `make check-web` passes.
- [ ] If it touches `internal/config` or `.env.example`, both are updated together.
- [ ] If it touches intent verification, signing, or the custody model, [docs/architecture.md](../docs/architecture.md) is updated to match.
- [ ] Commit messages follow Conventional Commits with a scope.
- [ ] No secrets, keys, or `.env` contents in the diff.
