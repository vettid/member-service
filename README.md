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

**Phase 2 in progress:** the member API connects members, signs them in
by email and lists the roster (row 2.2). The site and the CDK follow. The plan, its
decisions and its phases are in
[docs/MEMBER-SERVICE-PLAN.md](docs/MEMBER-SERVICE-PLAN.md).

## Layout

| Path | Contents |
|---|---|
| `web/` | Static site: HTML/CSS/JS, self-hosted assets, zero-tracker (see "Site") |
| `site-test/` | The site's tests: pages against a scripted API, and its CSP rules (Node's built-in runner, no dependencies) |
| `api/` | Go backend (`api/cmd/member-api`) |
| `contracts/` | Draft member and admin contracts |
| `cdk/` | Infrastructure (Phase 2) |
| `docs/` | Plan and runbook |

## Site

Plain HTML, CSS and JavaScript modules with no build step, served by
vettid.org's StaticSite construct (row 2.3) under its headers:
`script-src 'self'` and `style-src 'self'` with nothing inline, Trusted
Types (pages build nodes with `createElement` and `textContent` only),
and `Referrer-Policy: no-referrer`. The API is same-origin under `/api`.

| Page | |
|---|---|
| `/` | What the service is; Join or Sign in. A signed-in member goes home |
| `/connect/` | Join (§4.1): the email and its code, then the invitation (a button on a phone, a QR code drawn in the page on a computer), then the SAS to compare on the phone. A reload resumes the step |
| `/signin/` | Sign in (§4.2): the email, then the match code to compare on the phone. It reads the same whether or not the address is a member's |
| `/home/` | The member: name, join date, the member count, and for an admin the names |

`styles.css`, the fonts and logos, and `js/{dom,ui,qr,theme,theme-init}.js`
with the vendored QR encoder (MIT, `js/vendor/`) are copies from the
vettid.org account site (`sites/account`), so the two sites look alike;
keep them unchanged and refresh them from there. The member service's
own styles are in `member.css`.

## Member API

All routes are under `/api`, same-origin with the site. Sessions use
`__Host-` cookies that are `HttpOnly`, `Secure` and `SameSite=Strict`.
A `POST` must have `Content-Type: application/json`.

| Route | |
|---|---|
| `POST /api/connect/start` `{email}` | Emails a 6-digit one-time code and sets the flow cookie. Always `202 {expires_at}`, whether or not a code was sent. Rate limits: 3 codes per address per hour and 10 per day; 20 per client address per hour, where an IPv6 address counts as its /64. A source over its limit doesn't use up the address's codes. `bad_email` for something that is not an address |
| `POST /api/connect/verify` `{email, code}` | A right code (10 minutes, 5 tries, counted atomically) for the flow's address asks the service vault for an invitation with `email_verified: true`. Returns `{url, expires_at}`: the button on mobile, the QR code on desktop. Every refusal is `invalid_code`. A client address (an IPv6 /64) with 30 wrong codes in an hour is refused until the hour ends; only that source is held up. 200 wrong codes in an hour across all sources log an ERROR for an alarm, but block nobody |
| `GET /api/connect/status` | `{status}`: `code`, `waiting` (with `url`), `sas` (with `sas`, to compare on the phone), `connected` (the session cookie is set), `declined` or `expired` |
| `POST /api/signin/start` `{email}` | Asks the service vault to send a sign-in challenge to the member's vault. Returns `202 {match_code, expires_at}` and sets the sign-in cookie. The answer is the same whether or not the address belongs to a member: the service vault answers alike either way and applies its own limits per address and per source. Its source is a keyed hash of the client address (an IPv6 address counts as its /64), never the address itself. 30 sign-ins per client address per hour, then `429 rate_limited`. `bad_email` for something that is not an address |
| `GET /api/signin/status` | `{status}`: `pending` (with `match_code`, for the member to compare on the phone), `approved` (the session cookie is set, and any earlier session of the browser ends), `confirm` or `ended`. Denied and expired both read `ended`, so the answer never says whether an address is a member's: a member's vault can turn a challenge down without the member. The first outcome stands. An approval for a connection that has since been removed is `ended`. `confirm`: the member approved with a credential key the service vault doesn't recognise (`key_changed`, after a credential reset, a new phone, or someone else recovering the account). There is no session until the member proves the address too, and a `WARN` with `alarm: key_changed` is logged for the alarm |
| `POST /api/signin/confirm` `{email}` | In `confirm`: emails a 6-digit code to the address the sign-in started with (the one the service vault resolved to the connection). `202 {expires_at}`, with connect's per-address and per-source code limits (a limit holds the code back silently). `wrong_email` for another address |
| `POST /api/signin/verify` `{email, code}` | In `confirm`: the right code (10 minutes, 5 tries, connect's per-source wrong-code limit) starts the session: `{status: "approved"}`. Every refusal is `invalid_code` |
| `GET /api/me` | `{connection_id, name, joined, admin}`, or `401 signed_out` |
| `GET /api/members` | For a signed-in member: `{count}`. An admin also gets `members: [{name, joined}]`, oldest first. Connection ids are never included. Admins are the service vault's admin set, which the API keeps for a minute and replaces as soon as an `admin_set.changed` webhook arrives. When the service vault can't say, the answer is the count only. `401 signed_out` without a session |
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
node --test site-test/*.test.mjs   # the site (Node 22)
```

Requires Go 1.26 (`go.mod` pins the toolchain). Local configuration goes
in `.env`, which is never committed; see `.env.example`.

## License

AGPL-3.0. See [LICENSE](LICENSE).
