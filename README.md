# VettID Member Service

`member.vettid.org`: the member-governed side of vettid.org, covering
membership, proposals with secret-ballot voting, and the beta program
(cohorts, assignments, feedback). It is the first service built on
VettID. It connects with members through the
[VettID service vault](https://github.com/vettid/service-vault) using the
same SDK and webhooks a third party would use, so it also serves as the
reference integration.

Members connect once from a link or QR code. After that they sign in by
entering their email and approving on their phone. There are no
passwords, no magic links and no trackers.

## Status

**Phase 2 in progress:** the member API connects members (row 2.2).
Email sign-in, the roster, the site and the CDK follow. The plan, its
decisions and its phases are in
[docs/MEMBER-SERVICE-PLAN.md](docs/MEMBER-SERVICE-PLAN.md).

## Layout

| Path | Contents |
|---|---|
| `web/` | Static site: HTML/CSS/JS, self-hosted assets, zero-tracker |
| `api/` | Go backend (`api/cmd/member-api`) |
| `contracts/` | Draft member and admin contracts |
| `cdk/` | Infrastructure (Phase 2) |
| `docs/` | Plan and runbook |

## Member API

All routes are under `/api`, same-origin with the site. Sessions use
`__Host-` cookies that are `HttpOnly`, `Secure` and `SameSite=Strict`.
A `POST` must have `Content-Type: application/json`.

| Route | |
|---|---|
| `POST /api/connect/start` `{email}` | Emails a 6-digit one-time code and sets the flow cookie. Always `202 {expires_at}`, whether or not a code was sent. Rate limits: 3 codes per address per hour and 10 per day; 20 per client address per hour, where an IPv6 address counts as its /64. A source over its limit doesn't use up the address's codes. `bad_email` for something that is not an address |
| `POST /api/connect/verify` `{email, code}` | A right code (10 minutes, 5 tries, counted atomically) for the flow's address asks the service vault for an invitation with `email_verified: true`. Returns `{url, expires_at}`: the button on mobile, the QR code on desktop. Every refusal is `invalid_code`. A client address (an IPv6 /64) with 30 wrong codes in an hour is refused until the hour ends; only that source is held up. 200 wrong codes in an hour across all sources log an ERROR for an alarm, but block nobody |
| `GET /api/connect/status` | `{status}`: `code`, `waiting` (with `url`), `sas` (with `sas`, to compare on the phone), `connected` (the session cookie is set), `declined` or `expired` |
| `GET /api/me` | `{connection_id, name, joined}`, or `401 signed_out` |
| `POST /api/signout` | `204`, and the session ends |
| `POST /api/webhooks/service-vault` | The service vault's signed webhooks (SDK `Receiver`, with its replay cache in DynamoDB) |
| `GET /api/health` | `{status: "ok"}` |

**Email addresses are never stored.** A flow keeps a keyed hash of the
address (`MEMBER_API_PEPPER`) and of the code, and the browser sends the
address again with the code. The service vault binds the address to the
connection only because the code proved control of it (service-vault
README, "Email binding"). Logs carry ids, types and error codes, never
addresses, codes or tokens. Status requests ask the service vault
directly, at most every 2 seconds, so a late webhook doesn't hold up a
connection. A connection removed while its flow is still open gets no
session. Someone who forwards their own invitation link to another
person would bind their address to that person's phone. The SAS that
the site and the phone both show is the defence against this, so the
site asks the visitor to compare it.

### Configuration

From the environment (see `.env.example`):

- `MEMBER_API_ADDR`
- `SERVICE_VAULT_URL`, `SERVICE_VAULT_PIN` and `SERVICE_VAULT_API_KEY`
- `WEBHOOK_SECRET` and `MEMBER_API_PEPPER`, at least 32 bytes each
- `DYNAMODB_TABLE`, and `DYNAMODB_ENDPOINT` for local development
- exactly one of `MAIL_FROM` (SES, from a verified domain identity) and
  `MAIL_DIR` (local development: codes are written to files)
- `CLIENT_IP_HEADER`, which is `CloudFront-Viewer-Address` behind
  CloudFront. It needs `ORIGIN_VERIFY_SECRET` (at least 32 bytes): the
  secret the edge sends as `X-Origin-Verify`. Requests without it get
  `403`, except `/api/health`, so the origin can't be reached around
  the edge.

## Build and test

```sh
make build        # bin/member-api
make test         # go test ./...
make lint         # go vet + staticcheck
MS_IT_LOCALSTACK=http://127.0.0.1:4566 go test -tags integration ./api/internal/store/   # DynamoDB on LocalStack
```

Requires Go 1.26 (`go.mod` pins the toolchain). Local configuration goes
in `.env`, which is never committed; see `.env.example`.

## License

AGPL-3.0. See [LICENSE](LICENSE).
