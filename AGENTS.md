# Repository Instructions

- Assume there is no need for backwards compatibility, legacy support, or data migration from old implementations to new ways of doing things unless explicitly stated.
- Use conventional commits style for Git commit messages.
- When fixing a bug, add regression tests. Do not add regression tests for feature or behavior changes.

## Test Isolation

- Use `make test` or `make ci` for full suites.
- Run targeted tests through `scripts/test-env.sh`; for example, `scripts/test-env.sh go test ./internal/config -run TestLoadClient -count=1`.
- Do not run raw test commands, because Flow worker environment variables and configuration files can otherwise leak into the test process.

## Pull Requests

- Do not create draft pull requests. Only create pull requests that are ready for review.

## Command Output

- Never pipe output to `head` or `tail` without also saving the complete output to a file with `tee` so it can be searched without rerunning the command.
