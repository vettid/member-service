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
| `cdk/` | Infrastructure: the site, the member API, its table and alarms (see "Deployment") |
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

The files in `web/` are edited and tested as they are. At deploy, the
CDK app (`cdk/lib/site-build.ts`) publishes every JS and CSS file under
a name with its content hash (`js/connect.<hash>.js`), rewriting the
pages' `src`/`href` and the modules' static imports to match. A module's
hash covers its rewritten imports, so a change to `dom.js` also renames
every module that imports it. Hashed files are cached for a year as
immutable; pages revalidate on every visit, and fonts and images cache
for a week. A deploy therefore takes effect on a member's next page
load, never mixing old and new modules. Hashed files from earlier
deploys stay in the bucket, so a page loaded just before a deploy still
finds its files, and the pages are uploaded only after the new hashed
files. Only absolute, double-quoted `src`/`href` and relative static
imports are rewritten: the build refuses any other form that names a JS
or CSS file (a relative or single-quoted attribute, a query string, a
dynamic or absolute import, a CSS `@import`), and a reference to a file
that isn't there. The build is kept in `cdk/cdk.out/site-build/`.

## Member API

All routes are under `/api`, same-origin with the site. Sessions use
`__Host-` cookies that are `HttpOnly`, `Secure` and `SameSite=Strict`.
A `POST` must have `Content-Type: application/json`.

| Route | |
|---|---|
| `POST /api/connect/start` `{email}` | Emails a 6-digit one-time code and sets the flow cookie. Always `202 {expires_at}`, whether or not a code was sent. Rate limits: 3 codes per address per hour and 10 per day; 20 per client address per hour, where an IPv6 address counts as its /64. A source over its limit doesn't use up the address's codes. More than 60 starts from one client address in an hour get `429 rate_limited` before anything is written. The changed-key confirmation has its own per-address code limits, so connect starts can't use them up. `bad_email` for something that is not an address |
| `POST /api/connect/verify` `{email, code}` | A right code (10 minutes, 5 tries, counted atomically) for the flow's address asks the service vault for an invitation with `email_verified: true`. Returns `{url, expires_at}`: the button on mobile, the QR code on desktop. Every refusal is `invalid_code`. A client address (an IPv6 /64) with 30 wrong codes in an hour is refused until the hour ends; only that source is held up. 200 wrong codes in an hour across all sources log an ERROR for an alarm, but block nobody |
| `GET /api/connect/status` | `{status}`: `code`, `waiting` (with `url`), `sas` (with `sas`, to compare on the phone), `connected` (the session cookie is set), `declined` or `expired` |
| `POST /api/signin/start` `{email}` | Asks the service vault to send a sign-in challenge to the member's vault. Returns `202 {match_code, expires_at}` and sets the sign-in cookie. The answer is the same whether or not the address belongs to a member: the service vault answers alike either way and applies its own limits per address and per source. Its source is a keyed hash of the client address (an IPv6 address counts as its /64), never the address itself. 30 sign-ins per client address per hour, then `429 rate_limited`. `bad_email` for something that is not an address |
| `GET /api/signin/status` | `{status}`: `pending` (with `match_code`, for the member to compare on the phone), `approved` (the session cookie is set, and any earlier session of the browser ends), `confirm` or `ended`. Denied and expired both read `ended`, and not before 5 minutes from the start, when an unknown address's sign-in expires: a denial reads `pending` until then. So neither the answer nor its timing says whether an address is a member's. A member's vault can turn a challenge down without the member. The first outcome stands. An approval for a connection that has since been removed is `ended`. `confirm`: the member approved with a credential key the service vault doesn't recognise (`key_changed`, after a credential reset, a new phone, or someone else recovering the account). There is no session until the member proves the address too. A service vault that doesn't report `key_changed` counts as a change. When the approval is recorded, a `WARN` with `alarm: key_changed` (and the connection id) is logged once, for the alarm |
| `POST /api/signin/confirm` `{email}` | In `confirm`: emails a 6-digit code to the address the sign-in started with (the one the service vault resolved to the connection). The email says the credential changed, and never to share the code. A new code replaces the last one. The confirmation ends 30 minutes after the approval, however often a code is sent. `202 {expires_at}`, with connect's per-address and per-source code limits (a limit holds the code back silently). `wrong_email` for another address |
| `POST /api/signin/verify` `{email, code}` | In `confirm`: the right code (10 minutes, 5 tries per code, connect's per-source wrong-code limit) starts the session: `{status: "approved"}`. Every refusal is `invalid_code` |
| `GET /api/me` | `{connection_id, name, joined, admin}`, or `401 signed_out` |
| `GET /api/members` | For a signed-in member: `{count}`. An admin also gets `members: [{name, joined, suspended?}]`, oldest first. A member whose connection the service vault suspended (`connection.suspended`) isn't counted and is marked `suspended: true`; their sessions end, and connecting again replaces the entry. Connection ids are never included. Admins are the service vault's admin set, which the API keeps for a minute and replaces as soon as an `admin_set.changed` webhook arrives. When the service vault can't say, the answer is the count only. `401 signed_out` without a session |
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
  the edge. `ORIGIN_VERIFY_SECRET_PREVIOUS` (optional, at least 32
  bytes) is accepted as well, for a rotation (see "Rotating the origin
  secret").

## Deployment

One CDK stack, `MemberService-<env>` (`cdk/`), in us-east-1 and in the
same account as the service vault's stack `ServiceVault-<env>`. It reads
that stack's values from the SSM parameters it publishes
(`/vettid/service-vault/<env>/…`). They are read at deploy time, not
imported, so the service vault's stack stays free to change them. After
such a change, deploy this stack again. Nothing is deployed from CI. CI publishes the image
to GHCR from `main`, and a deployment pins its digest. Check the digest's provenance first
([Verifying an image](#verifying-an-image)).

What the stack makes:

- **The site:** `web/` on S3 and CloudFront at `hostName`, using
  vettid.org's StaticSite construct (`cdk/lib/static-site.ts`, a copy of
  vettid.org `lib/constructs/static-site.ts` with two additions:
  `apiForwardHeaders`, for the webhook signature headers, and
  `hashedAssets`, for the content-hashed JS and CSS described under
  "Site"). It has
  that construct's CSP, headers and branded 404, and a certificate in the
  zone.
- **`/api/*`:** CloudFront forwards it to an API Gateway HTTP API, adding
  `X-Origin-Verify` (a generated secret) and `CloudFront-Viewer-Address`,
  and passing the webhooks' `VettID-Signature` and `VettID-Event-Id`.
  The HTTP API reaches the task through a VPC link and Cloud Map
  (`member-api`, SRV, in the service vault's namespace). There's no load
  balancer.
- **The member API:** one ARM64 Fargate Spot task (0.25 vCPU / 512 MB)
  in the service vault's public subnets, with a public IP for its
  outbound calls and no NAT gateway. Its security group admits only the
  VPC link. It sends out only on 443 and to the service vault's API port,
  and the stack adds the matching ingress rule on the service vault's
  group.
- **Data:** one DynamoDB table (`pk`/`sk`, TTL on `exp`, PITR, deletion
  protection, retained).
- **Secrets:** generated by the stack and never in code:
  - the API key the member API presents to the service vault;
  - the pepper;
  - the origin-verify secret.

  The webhook secret and the TLS pin come from the service vault's stack.
- **Alarms:** one per log line the member API marks with `alarm`, plus ones for errors and for the service's tasks:
  - `verify_failed`: wrong codes reached the hourly alarm level;
  - `key_changed`: a sign-in was approved with a changed credential key.
  - `errors`: 5 or more ERROR lines in 15 minutes that no alarm above
    covers (DynamoDB, SES or the service vault failing).
  - `down`: no member API task has run for 10 minutes. It watches the
    ECS service's CPU samples, which ECS reports without Container
    Insights.
  - `task_stopped`: a task stopped on its own: it exited, failed its
    health check or failed to start. A crash loop keeps it raised, while
    `down` may stay quiet because short-lived tasks still report CPU.
    Stops by a deploy, a scale-in, a Spot reclamation or an operator
    (stop codes `ServiceSchedulerInitiated`, `SpotInterruption`,
    `UserInitiated`) don't raise it, except a failed health check.
  - `deploy_failed`: the circuit breaker failed a deployment and rolled
    it back.

  ECS's own events for this cluster go to a log group of their own,
  kept a month, through one EventBridge rule. That rule adds an
  account-wide CloudWatch Logs resource policy (10 per region).

  They always notify someone. The stack requires either an SNS topic
  (`alarmTopicArn`) or an address (`alarmEmail`), and won't synth
  without one.

Context (`-c key=value`):

| Key | Required | |
|---|---|---|
| `envName` | no (`prod`) | Part of every name, and of the service vault's parameter path |
| `image` | yes | `ghcr.io/vettid/member-service@sha256:<digest>`, never a tag |
| `hostName` | yes | e.g. `member.staging.vettid.org` |
| `hostedZoneId`, `zoneName` | yes | The zone the host is in. For staging that's `staging.vettid.org`, whose id vettid.org publishes in SSM at `/vettid-org/staging/dns/zone-id` |
| `mailFrom` | yes | A sender of a verified SES domain identity, e.g. `no-reply@staging.vettid.org` |
| `serviceVaultParams` | no | The service vault's SSM path, if not `/vettid/service-vault/<env>` |
| `originSecretSerial` | no | Only for "Rotating the origin secret": a value different from the last deploy's makes CloudFront send the secret's current value |
| `alarmTopicArn` or `alarmEmail` | one of them | Where the alarms go. Either an SNS topic that reaches a person, or an address for a topic of the stack's own; confirm that subscription from the email AWS sends. For staging, use vettid.org's vault alerts topic, which emails the admin address: `aws ssm get-parameter --name /vettid-org/staging/vault/alerts-topic-arn` |

First deployment:

1. The service vault: `ServiceVault-<env>` is deployed from a
   service-vault `main` that publishes its SSM parameters (since
   service-vault #29), and its task has started at least once. Only then does its TLS pin parameter hold a pin:
   `aws ssm get-parameter --name /vettid/service-vault/<env>/tls-pin`
   must answer `sha256/…`, not `unset`. With `unset`, the member API exits
   at start and the stack's create rolls back.
2. The availability zones: HTTP API VPC links aren't supported in a few
   AZ IDs (see the API Gateway documentation for the current list, e.g.
   `use1-az3`). Check the AZ IDs of the service vault's two subnets
   (`aws ec2 describe-subnets`, `AvailabilityZoneId`) before deploying.
3. The image: after the first publish, check that the GHCR package
   `vettid/member-service` is public. Fargate pulls it without
   credentials, as it does the service vault's.
4. SES: `mailFrom`'s domain must be a verified identity in the account.
   While the account is in the SES sandbox, codes reach only verified
   recipients.
5. `cd cdk && npm ci && npx cdk deploy -c envName=staging -c image=… -c hostName=member.staging.vettid.org -c hostedZoneId=… -c zoneName=staging.vettid.org -c mailFrom=no-reply@staging.vettid.org -c alarmTopicArn=<the vault alerts topic>`
6. Register the API key with the service vault:
   - Add the SHA-256 (hex) of the `ServiceVaultApiKeySecretArn` output's
     value to the service vault's `…/api-key-hashes` secret. The value is
     comma-separated, so keep the keys already there.
   - Restart the service vault, which reads the hashes at start. Until
     then, the member API's calls are refused (`unauthorized`).
   - Don't also deploy the service vault with `apiClientSecurityGroupId`
     set to this task's group: this stack already adds that rule.
7. Point the service vault's webhooks here: redeploy it with
   `-c webhookUrl=` the `WebhookUrl` output
   (`https://<hostName>/api/webhooks/service-vault`).
8. Check `https://<hostName>/api/health` and the site, and connect a
   test member.

If a first create rolls back, it keeps what is retained:
- the table (deletion-protected);
- the API key and pepper secrets;
- the log groups.

They have CloudFormation-made names, so a retry doesn't collide with
them. Delete the leftovers once the stack is up (turn off the table's
deletion protection first).

Later:
- **The pin:** the member API reads the TLS pin at start. If the service
  vault's key ever changes, restart the member API with
  `aws ecs update-service --force-new-deployment` (the `ClusterName` and
  `ServiceName` outputs).
- **The origin-verify secret:** see "Rotating the origin secret" below.
- **Throttling:** the HTTP API's stage limit (50 requests/s, burst 100)
  is shared by every caller. Its execute-api URL is public, so requests
  sent there directly, which the member API refuses without the origin
  secret, count against it too. That's accepted for staging. If it is
  ever abused, put a WAF rate rule on the execute-api stage and raise the
  limit.

### Rotating the origin secret

CloudFront adds `X-Origin-Verify` to every `/api/*` request, and the
member API refuses requests without it. The value lives in two places
that change at different times:
- CloudFront reads it from Secrets Manager only when a deploy changes the
  distribution;
- the member API reads it when its task starts.

So the API also accepts a previous value (`OriginPreviousSecretArn`),
and a rotation moves through it, with no window in which requests fail.
Rotate the secret when it may have leaked (at once), when someone who
could read it leaves, or yearly.

The commands never print the secret. Values go through a pipe, and
`tr -d '\n'` drops the newline `--output text` adds. With `S`, `P`, `C`
and `N` the `OriginSecretArn`, `OriginPreviousSecretArn`, `ClusterName`
and `ServiceName` outputs:

1. Keep the current value as the previous one:
   `aws secretsmanager get-secret-value --secret-id S --query SecretString --output text | tr -d '\n' | aws secretsmanager put-secret-value --secret-id P --secret-string file:///dev/stdin`
2. Make a new current value:
   `aws secretsmanager get-random-password --password-length 48 --exclude-punctuation --query RandomPassword --output text | tr -d '\n' | aws secretsmanager put-secret-value --secret-id S --secret-string file:///dev/stdin`
3. Restart the member API, which then accepts both values:
   `aws ecs update-service --cluster C --service N --force-new-deployment`,
   then `aws ecs wait services-stable --cluster C --services N`.
   CloudFront still sends the old value, which is now the previous one,
   so the site keeps working. Check that the sign-in page reaches
   `/api`.
4. Make CloudFront send the new value. Deploy the stack with its usual
   context plus `-c originSecretSerial=$(date -u +%Y%m%d%H%M)`. That
   adds an `X-Origin-Verify-Serial` header, which the member API
   ignores. The change makes CloudFormation update the distribution and
   read the secret again. The deploy ends once the distribution is
   deployed. Edge locations can serve the old configuration for a few
   minutes more, so wait 15 minutes. Later deploys may leave
   `originSecretSerial` out: the distribution then reads the same,
   current value.
5. Check that CloudFront has moved: the member API logs `request with
   the previous origin secret` (at most once a minute) while requests
   still carry the old value. In the `LogGroupName` output's group,
   none may have appeared for the last 15 minutes, with traffic on the
   site (load the sign-in page). If one has, wait and look again, or
   deploy again with a new `originSecretSerial`.
6. Retire the old value. Give `P` a fresh random value (step 2's
   command with `--secret-id P`), and restart the member API again (step
   3). Check the sign-in page once more.

If the secret leaked, go through the steps without waiting longer than
steps 4 and 5 need: until step 6, the old value still opens the origin.

### Verifying an image

CI's `publish` job signs build provenance for each image it pushes
(SLSA v1, GitHub artifact attestations on Sigstore's public instance),
stored next to the image in GHCR. Check it before a digest goes into a
deployment:

```sh
make verify-image IMAGE=ghcr.io/vettid/member-service@sha256:<digest>
```

It runs `gh attestation verify` (GitHub CLI) and passes
only for an image built by this repository's `ci.yml` from `main` on a
GitHub-hosted runner. The output names the source commit; it should be
the one being deployed. Images published before provenance was added
have none and fail the check.

## Build and test

```sh
make build        # bin/member-api
make test         # go test ./...
make lint         # go vet + staticcheck
MS_IT_LOCALSTACK=http://127.0.0.1:4566 go test -tags integration ./api/internal/store/   # DynamoDB on LocalStack
node --test site-test/*.test.mjs   # the site (Node 22)
(cd cdk && npm ci && npm run typecheck && npm test)   # the stack
```

Requires Go 1.26 (`go.mod` pins the toolchain). Local configuration goes
in `.env`, which is never committed; see `.env.example`.

## License

AGPL-3.0. See [LICENSE](LICENSE).
