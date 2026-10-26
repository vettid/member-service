# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

`member.vettid.org`: a static site (`web/`) and a Go backend (`api/`)
for membership, proposals and secret-ballot voting, and the beta
program. It talks to the VettID service vault only through its Go SDK
and signed webhooks, the way any third-party application would. It holds
sessions and member-authored content; it holds no VettID keys and no
member secrets.

The plan, its decisions and its phases are in
[docs/MEMBER-SERVICE-PLAN.md](docs/MEMBER-SERVICE-PLAN.md). Read it
before starting a PR and do not reopen its decisions.

## Build and test

```sh
go build -o bin/member-api ./api/cmd/member-api
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
go test -race -count=1 ./...
```

## Conventions

- Backend: standard library first (`net/http`, `log/slog`, `flag`), plus
  the service vault's `sdk/go`. No frameworks.
- Site: plain HTML/CSS/JS with no build step and no third-party scripts,
  fonts or analytics. It must work under the StaticSite construct's
  headers in vettid.org (`script-src 'self'`, no `unsafe-inline`,
  Trusted Types, `Referrer-Policy: no-referrer`).
- The member service never sees VettID keys. Sign-in, completions and
  votes are approved on the phone and signed in the member's vault; the
  member service learns the outcome from service-vault webhooks, which it
  verifies before acting on them.
- Logs carry ids, types, sizes and error codes, never emails, match
  codes, session tokens or member content.

## Coordination rules

1. **Scope.** Work in this repository. Read service-vault, vettid-vault,
   vettid-relay and vettid.org; open PRs there only when the owner
   assigns them.
2. **Small PRs**, one per row of the plan's §10, each with its tests
   passing locally before it is pushed. Titles name the row
   (`2.2: member API sessions`). The owner merges; the agent never
   merges.
3. **Public text** (commits, PRs, issues, code comments) never mentions
   reviews, finding ids, private files or local paths outside the
   repository. Cite specifications by document and section.
4. **No secrets in the repository**: no keys, API keys, webhook secrets
   or member data. `.env` stays local, and gitleaks runs in CI.
5. **Commit trailers and PR bodies**: as the harness provides them; no
   other attribution.
