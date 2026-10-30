## What changed
`impl.Do` in `app/app.go` changed one line.

## Risk
`impl.Do` is high risk: it runs a shell and has no test.

## Before merging
- [ ] Add a test that reaches `impl.Do`.
- [ ] Confirm the input is not passed to `sh -c`.
