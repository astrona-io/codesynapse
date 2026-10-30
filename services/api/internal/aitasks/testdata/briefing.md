## What this is
A small Go service that runs commands for an HTTP API.

## Architecture
`app` holds the request handlers; `impl.Do` runs the command.

## Quality and risk
One critical finding: `impl.Do` passes input to `sh -c`.

## Recommended next steps
1. Replace `sh -c` in `impl.Do` with `exec.Command` and fixed arguments.
2. Add a test for `impl.Do` with hostile input.
