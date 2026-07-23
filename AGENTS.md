# Repository Instructions

- Assume there is no need for backwards compatibility, legacy support, or data migration from old implementations to new ways of doing things unless explicitly stated.
- Use conventional commits style for Git commit messages.
- When fixing a bug, add regression tests. Do not add regression tests for feature or behavior changes.

## Test Isolation

- Go tests are hermetic by construction: every test package's TestMain routes through `internal/testenv`, which clears the environment and substitutes temporary HOME/XDG/TMP directories before any test runs. Plain `go test ./...` and targeted runs like `go test ./internal/config -run TestLoadClient -count=1` are both safe.
- New test packages must add a `testmain_test.go` containing `func TestMain(m *testing.M) { testenv.Main(m) }`; a root-level test enforces this.

## Pull Requests

- Do not create draft pull requests. Only create pull requests that are ready for review.

## Command Output

- Never pipe output to `head` or `tail` without also saving the complete output to a file with `tee` so it can be searched without rerunning the command.
