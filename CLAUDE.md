# zitadel-netbird-sync

Small Go service that mirrors Zitadel projects and their members into NetBird
groups. Public repository: nothing in it may point to a specific organization.

## Commands

```sh
go test -race -count=1 ./...   # all tests
go vet ./... && gofmt -l .     # must print nothing
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
docker build -t zns:local .
```

## Layout

- `main.go` — configuration, the run loop, dry-run output.
- `internal/config` — environment variables, validation, safe defaults.
- `internal/zitadel` — read-only client: projects, user grants, verified emails.
- `internal/netbird` — groups, users, setting a user's group list.
- `internal/syncer` — `BuildPlan` (pure, no I/O) and `Apply`.

## Rules that must hold

- **Ownership by prefix.** A group is touched only if its name starts with
  `GROUP_PREFIX`. Never widen this, and never run with an empty prefix.
- **Never delete groups, never create or block users, never change roles.**
- **Dry run is the default.** Only an explicit `DRY_RUN=false` writes.
- **Fail closed on doubtful input.** No projects, an incomplete page or too many
  removals stop the run without changes.
- **Standard library only.** A new dependency needs a strong reason.
- **No personal data or secrets in logs or errors.** Log NetBird user IDs and
  group names, never email addresses, tokens or response bodies.
- **GitHub Actions are pinned by commit hash** with the version as a comment.

## Working here

- Keep the plan logic in `BuildPlan` free of I/O so it stays table-testable.
- Every behaviour change gets a test first; the suite must stay green with
  `-race`.
- Examples and tests use neutral names only (`example.org`, `idp-alpha`).
- Documentation and commit messages are in English.
- Read `CLAUDE.local.md` if it exists: it holds local context that is not part
  of the repository.
