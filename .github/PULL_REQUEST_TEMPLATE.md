## What and why

<!-- What changes, and why. Link the issue: Fixes #123 -->

## How it was tested

<!-- Tests added or changed; anything checked by hand. -->

## Checklist

- [ ] `make check` passes (gofmt, vet, guard tests, `go test -race`)
- [ ] Tests cover the change (a regression test for a bug fix)
- [ ] New tool results with outside content are flagged `Untrusted`; new
      capabilities have the right risk level; new tables with `user_id` are in
      the cascade-delete test
- [ ] Docs and `CHANGELOG.md` (Unreleased) updated if behaviour changed
- [ ] No secrets, personal data or local paths in code, tests or docs
- [ ] AI tools were used: yes / no (if yes, I reviewed and tested every line)
