# vettid.org Member Service & Service Vault — Plan

**Version:** 0.7.0 (draft)
**Status:** Planning — Phase 1 targeted before 2026-11-15
**Owner:** Al Liebl
**Repos:** `vettid/service-vault` (new; replaces the retired `vettid/vettid-service-vault`), `vettid/member-service` (new), `vettid/vettid-vault` (member vault: service connections, contracts, votes), `vettid/vettid-relay` (`relayclient`), `vettid/vettid.org` (specs), `vettid/vettid-android`, `vettid/vettid-ios`

---

## 1. Purpose

Two deliverables, in dependency order:

1. **Service vault** — rebuilt from scratch as a VMS v2 peer on the relay, with connect-link onboarding, email lookup sign-in, admin approvals and verifiable voting. It is what any application uses to connect with VettID users, so it must be cheap and ordinary to run: one container, no enclave, `cdk deploy`. The previous NATS prototype (`vettid/vettid-service-vault`) is retired and archived; no code carries over.
2. **Member service** — `member.vettid.org`, the first connected service built on VettID. It runs the member-governed side of vettid.org: membership, proposals with verifiable voting, and the beta program (cohorts, assignments, feedback). It integrates with the service vault exactly the way a third party would — through the SDK and webhooks — so it doubles as the reference integration.

Both rest on member-vault support for service connections, contracts and votes, which VAULT-MESSAGING previously placed out of scope (lifted by decision, 2026-10-26).

## 2. What it proves

| Claim | How the member service shows it |
|---|---|
| A service can authenticate users with no password and no secret-bearing user database | Members connect once from a link on the site; after that they sign in by entering their email and approving on the phone. |
| Connecting to VettID users is cheap and ordinary | The service vault is one small container at ~$20/month, and the member service talks to it through the same SDK anyone else would use. |
| Delegated access is scoped and revocable | The member contract grants only what the service needs; a member can sever it from the phone. |
| A vote can be public to check without exposing members | Every ballot and the tally are published under per-proposal pseudonyms; anyone can check that each eligible member voted at most once and that the count is right, and each member checks their own ballot (§8). |
| Every action traces to the user who authorized it | Completions and approvals are signed by the member's vault and recorded in the service vault's audit log. Votes too: each ballot is signed with the member's credential key in the organisation's record, and published only under a pseudonym. |

## 3. Architecture

Three parties. The member service is an ordinary application; the service vault is the VettID-aware component it integrates with; the member's vault and phone are VettID core.

```
┌─────────────────────────────────┐
│ MEMBER                          │
│  VettID app (phone)             │
│   approve connect / sign-in /   │
│   completions; vote UI;         │
│   sever contract                │
│          │ VMS session          │
│  Member's vault (Nitro)         │
│   Protean Credential; signs     │
│   contracts, challenges,        │
│   completions; signs and casts  │
│   ballots                       │
└──────────────┬──────────────────┘
               │ relay — VMS v2 peer protocol (suite 2)
┌──────────────┴──────────────────┐
│ VETTID.ORG SERVICE VAULT        │
│  service-vault daemon           │
│  (Fargate, one task)            │
│   service identity (KMS env.)   │
│   VMS v2 peer: mailbox,         │
│     handshake, conn tokens      │
│   connections + contracts       │
│   email index (HMAC + pepper)   │
│   connect, sign-in, approvals,  │
│     completions, ballot         │
│   audit log                     │
│          ▲          │           │
│   SDK API│          │webhooks   │
└──────────┼──────────┼───────────┘
           │          ▼
┌──────────┴──────────────────────┐
│ MEMBER SERVICE                  │
│  member.vettid.org              │
│   web: static site (S3/CF)      │
│   api: Go backend (Fargate)     │
│     sessions, proposals,        │
│     cohorts, assignments,       │
│     comments, results (DDB)     │
└─────────────────────────────────┘
```

**Integration contract between the member service and the service vault** — the same one a third party gets:

- The member service calls the service vault through the Go SDK over a private HTTPS endpoint with an API key (never internet-exposed). API auth is on unless explicitly disabled for local development.
- The service vault calls the member service back with webhooks signed by HMAC over `timestamp.body` (connection accepted, sign-in approved/denied, completion signed, ballot published).
- The member service holds no VettID keys and no member secrets; it holds sessions and member-authored content.

**How the service reaches the member:** a service cannot push to a phone — only attested member vaults can wake one (PUSH-GATEWAY). Every request (sign-in, assignment, governance deadline) is a message to the member's vault over the connection; the member's vault turns it into a feed event, which wakes the app per its own feed-kind → channel mapping and priority.

**Trust boundaries:**

- Member secrets live only in the member's vault. Anything signed with the credential key — contract acceptance, sign-in, completions, the voting key — happens there, not on the phone; the phone approves.
- The service vault holds the *service's* keys (operator-readable at runtime, like any service's own keys), connection records and contracts. That is why it runs in a plain container while member vaults run in Nitro.
- Ballot secrecy is cryptographic (§8) against the service; secrecy against network metadata held by a single operator is a separate boundary (§8, "Operator metadata").

## 4. Flows

### 4.1 Connect (first visit)

1. Visitor on member.vettid.org enters their email and clicks **Connect with VettID**.
2. Member service asks the service vault for a connection invite, passing the email. The service vault returns a VMS v2 invite link (claim id, bundle commitment, one-shot open token). The site shows it as a button on mobile (deep link into the VettID app) and as a QR on desktop.
3. The VettID app fetches the bundle via the member's vault, verifies the commitment, and shows who is connecting — and, once contracts land (Phase 3), the member contract (§5).
4. Member accepts; the connection completes over the suite-2 handshake. The service vault records the connection, binds the email entered in step 1 to it as an HMAC under a server-side pepper, and webhooks the member service.
5. Member service creates the session. Connecting *is* the first sign-in.

The email is entered on the site, not disclosed by the vault: the member vault never sends the account email to a connection (VAULT-MESSAGING; `vault/account.go`). Binding is safe because sign-in still requires phone approval — a wrong email only means the member can't use it to sign in.

### 4.2 Sign-in (returning member)

1. Member enters their email on member.vettid.org. The site shows a two-digit match code and waits.
2. Member service sends the email to the service vault, which computes the HMAC, resolves it to a connection — or not — and returns the same response after the same delay either way.
3. If a connection exists, the service vault sends a `connection.authenticate.challenge` to the member's vault with `context` = "Sign in to vettid.org member service — match code NN" and an envelope `exp` of five minutes. The member's vault raises an approval on the phone.
4. Member confirms the code matches the browser and approves; the member's vault signs the challenge with the credential key.
5. Service vault verifies and webhooks the member service, which issues a short-lived session.
6. If the approval came with a credential key the service vault does not recognise (`key_changed`: a credential reset, a new phone, or an account recovered by someone else), the member service issues no session until the member also enters a code emailed to the address, and alerts (owner decision 2026-10-27).

Controls: one pending request per member, five-minute TTL, per-email and per-source rate limits; denying on the phone invalidates the challenge; an unregistered email produces the same screen as a registered one; the member vault's ask gate (mute, cooldown, rate) applies. QR remains as a fallback for a member not receiving push. No passwords, no magic links.

### 4.3 Vote

Member sees an open proposal on the site; the vote itself happens in the app and the member's vault (§8): the vault signs the ballot and sends it over the member's connection, keeps the signed receipt, and checks its row in the published results after close.

### 4.4 Assignment

Member service sends an assignment to a cohort through the service vault, which delivers it to each member's vault as a feed event. The member completes it and approves the completion on the phone; the member's vault signs it; the service vault verifies and webhooks the member service, which updates the dashboard.

### 4.5 Governance action (admin)

Creating a proposal, opening or closing a vote, changing a contract template or the admin set, or pushing an assignment requires admin approval signed by an admin's vault under an admin contract. Until contracts land, approval is a `connection.authenticate.challenge` whose `context` names the action and carries its hash. The threshold is m-of-n with m a configuration value: **v1 runs 1-of-n** (any one admin approves) and can be raised to 2-of-n without a code change. No admin passwords.

## 5. Contracts

Contracts arrive in Phase 3 with member-vault support (§10). Until then the member service treats every connection as a provisional member with the scopes below implied.

### 5.1 Member contract (`member-v1`)

| Scope | Rule |
|---|---|
| Disclosure | Display name; membership status. Email is entered on the site and held by the service only as an HMAC for sign-in lookup |
| Sign-in | Service may send sign-in challenges when the member's email is entered |
| Voting | Service may present proposals and certify one voting key per proposal |
| Beta program | Service may assign tasks and request feedback; member signs completions |
| Messaging | `general` and `message` class notifications; `urgent` for governance deadlines only |
| Status | **provisional** (beta, non-binding votes) converts in place to **member** when a subscription is paid |
| Revocation | Member severs from phone at any time; service may revoke on membership lapse |

### 5.2 Admin contract (`admin-v1`)

Adds: may approve governance actions as one of n admins. Issued only to the admin set. v1 threshold is 1-of-n; initial set is Al, with admins added as members join. Every approval is signed and audited regardless of threshold.

## 6. Member service features

- **Membership:** roster = connections (later: connections with an active member contract). Everyone is provisional during the beta. Members see only the roster's size; admins also see names and join dates (owner decision 2026-10-27). A connection the service vault suspends (`connection.suspended`, after a rotation conflict, VAULT-MESSAGING §3.4) stays on the roster marked for admins, isn't counted, and its sessions end; the member's next connection replaces it (chosen; not in the spec). Paid status needs payments (not yet built) and a service-callable status route on the member API (does not exist; `GET /api/account/me` is member-session only) — both later.
- **Proposals and voting:** proposal = title, body, options, record date, open/close (window ≤ 7 days), quorum rule, binding flag; signed by the service vault; pseudonymous ballot per §8; one vote, final (decided); threaded comments, members only. Binding votes must match the LLC operating agreement — counsel review before the first binding vote.
- **Beta program:** cohorts invited in waves; assignments as task cards with signed completions and optional structured feedback; bugs route to a GitHub issue template; admin dashboard of completion and activity derived from signed completions.
- **Notifications:** delivered as feed events from the member's vault; categories map onto existing feed kinds and priorities (`urgent` exists). A notification/sound spec for service events is to be written — NOTIFICATION-SOUND-SPEC.md does not exist yet.
- **Discussion:** scoped comments built natively; free-form discussion on GitHub Discussions during the beta; Discourse with VettID SSO later, optional, never the system of record.
- **Data:** zero-tracker static site, self-hosted assets; members export their own content; the Project exports everything; beta feedback deleted 6 months after beta close; governance records retained per the operating agreement.

## 7. Repository organization

### 7.1 `vettid/vettid-relay` (existing)

`relayclient` is the shared Go client, already used by vettid-vault and vettid-agent. Ephemeral-key deposits already work (`relayclient.New(base, freshKey)`; open tokens need no registered sender). Changes: `crypto/rand` for token `jti`s and unit tests. Votes need no relay change (§8).

### 7.2 `vettid/service-vault` (new)

A new repository; the NATS prototype in `vettid/vettid-service-vault` is retired and archived, and none of its code is kept. Target layout:

```
service-vault/
├── README.md  CLAUDE.md  LICENSE  Makefile
├── docs/
│   ├── SERVICE-VAULT.md          architecture, trust model
│   ├── BALLOT-PROTOCOL.md        §8 in full
│   ├── DEPLOYMENT.md  SDK.md
│   └── api/openapi.yaml
├── cmd/service-vault/
├── internal/
│   ├── peer/       VMS v2 peer side: relay mailbox, suite-2 handshake,
│   │               connection tokens, connection.removed
│   │               (imports vettid-vault vms/* + relayclient, as vettid-agent does)
│   ├── connect/    invites, connection lifecycle, email index
│   ├── signin/     lookup + connauth challenges, match codes
│   ├── approval/   admin m-of-n
│   ├── contract/   offers, acceptance, completions (Phase 3)
│   ├── ballot/     proposals, tags, ballots, receipts, results (Phase 5)
│   ├── keystore/   KMS envelope (AES-GCM data keys), persisted;
│   │               Ed25519, suite-2 KEM, P-256 (anchor key)
│   ├── store/      DynamoDB: connections, pending challenges, audit
│   ├── webhook/    HMAC over timestamp.body, retries
│   ├── api/        private HTTPS API, API keys
│   └── audit/
├── sdk/go/         Node and Python after the member service ships
├── cdk/            public subnets, egress-only SGs, no NAT, DynamoDB
│                   gateway endpoint only, 1 Spot task, Insights off,
│                   private API endpoint
└── .github/workflows/  gitleaks, ci, release
```

### 7.3 `vettid/member-service` (new)

```
member-service/
├── README.md
├── docs/
│   ├── MEMBER-SERVICE-PLAN.md    this document
│   └── RUNBOOK.md                cohort ops, proposal lifecycle, release
├── contracts/
│   ├── member-v1.json
│   └── admin-v1.json
├── web/                static member.vettid.org — HTML/CSS/JS, self-hosted
│                       assets, zero-tracker; headers per vettid.org's
│                       StaticSite construct (Trusted Types, no-referrer)
├── api/                Go backend (stdlib + service-vault sdk/go)
│   ├── cmd/member-api/
│   └── internal/
│       ├── session/   connect + sign-in via SDK; webhook receiver
│       ├── members/   roster, status
│       ├── proposals/ lifecycle, results display, Merkle proof UI
│       ├── beta/      cohorts, assignments, completions, feedback
│       ├── comments/
│       ├── admin/     approval orchestration
│       └── store/     DynamoDB
├── cdk/                StaticSite (web), Fargate task (api),
│                       DynamoDB tables, API Gateway
├── scripts/            setup, reset-cohort, publish-results
├── .env.example  .gitignore  .gitleaks.toml
└── .github/workflows/  gitleaks, ci, deploy
```

### 7.4 Member vault, specs and apps (existing)

- **vettid.org `docs/VAULT-MESSAGING.md`:** remove "B2C service vaults" and "votes" from §1.1 out-of-scope (approved 2026-10-26); add service connections and contracts (Phase 3) and votes (Phase 5), with wire vectors.
- **vettid-vault:** service invite kind or contract in the bundle; contract acceptance, storage and sever; `completion.sign`; the vote feature (§8). New optional features go through `features/all` and the release config.
- **vettid-android / vettid-ios:** match code shown on the authentication approval; contract review; completion approval; vote and verify-my-vote UI. Android first; iOS follows and needs APNs for push.

## 8. Voting (v1)

The protocol is the service vault's `docs/BALLOT-PROTOCOL.md` (0.10.0 and later), written and reviewed before ballot code. This is the plan-level summary. **It replaces the secret-ballot design of plan 0.6.x** (owner decision 2026-10-29): votes are pseudonymous, not secret, and need no blind signatures, ballot mailbox, token pool or relay change.

**Requirements:** eligibility (the roster at the record date), one vote per member, final; pseudonymous publication (no names, emails or connections, and no link across proposals); transparent and verifiable counting. The service and the organisation know how each member voted and keep that record. Receipt-freeness and coercion resistance are out of scope.

**Mechanism:**

1. *Propose:* the service vault signs the proposal, anchors it in Sigstore's Rekor log and publishes it on votes.vettid.org. Each eligible member gets a random per-proposal tag in `vote.proposal`; the proposal commits to all tags by a Merkle root.
2. *Cast:* the member's vault makes an Ed25519 key for this proposal only, signs `{proposal, tag, choice}` with it, signs the same ballot with the member's credential key (kept unpublished in the organisation's record), and sends it over the member's connection. The service checks the tag and one ballot per tag, and answers with a signed receipt.
3. *Publish:* at close, every tag with its ballot or "didn't vote", and the tally, signed, anchored and published on votes.vettid.org.
4. *Verify:* anyone checks the signatures, one ballot per tag and the tally; each member's vault checks its own row and holds its receipt. A disputed row is settled from the organisation's record, which holds the member's credential signature.

**Members are told before they vote:** the app shows a fixed sentence on every ballot screen, before the member approves: the organisation sees how they vote and keeps that record; the public results show only an anonymous code. The wording is the app's, not the organisation's, so no proposal can leave it out.

**Limits, stated in the protocol:** the service sees how members vote and the running tally; it could vote for members who never check, or add phantom members before the record date, which only the published counts and outside knowledge show.

## 9. Deployment and cost

Both services run as single ARM64 Fargate tasks in one VPC with public subnets and egress-only security groups (no NAT Gateway), the free DynamoDB gateway endpoint and no interface endpoints (each costs ~$7/AZ/month), Spot where restarts are harmless (pending challenges are in DynamoDB, so they survive), and Container Insights off. The service vault's API is reachable only from the member API; the public surface is CloudFront (web) and API Gateway (member API).

| Item | Monthly |
|---|---|
| Service vault task (0.25 vCPU / 512 MB) | ~$8 on-demand, ~$3 Spot |
| Member API task (0.25 vCPU / 512 MB) | ~$8 on-demand, ~$3 Spot |
| DynamoDB (on-demand), KMS, API Gateway, S3/CloudFront, ECR, logs, transfer | ~$12 |
| Relay / push gateway | existing |
| **Total** | **~$28 on-demand, ~$18 Spot** (≈ $0.09 per member at 300) |

One-time: cryptographic review of the ballot code ($0 via academic peer review to ~$5–15K commissioned); counsel on the operating-agreement alignment (within the formation engagement).

## 10. Implementation plan

Phase 1's gate stays the goal for 2026-11-15; items are ordered so that sign-in works first and admin approvals follow.

### Phase 0 — Foundation (2–3 days)

| ID | Work item | Repo | Done when |
|---|---|---|---|
| 0.1 | Create `service-vault` repo per §7.2; retire and archive `vettid-service-vault` | service-vault | CI green on skeleton |
| 0.2 | Create `member-service` repo with layout §7.3, gitleaks local + CI, `.env.example`, README | member-service | CI green on skeleton |
| 0.3 | VAULT-MESSAGING: lift "B2C service vaults" and "votes" out of §1.1 scope | vettid.org | Version bumped |
| 0.4 | `relayclient`: `crypto/rand` jtis, unit tests | vettid-relay | Tests pass |
| 0.5 | Draft `member-v1.json` and `admin-v1.json` contracts | member-service | Reviewed against the contract model |

### Phase 1 — Service vault: connect, sign-in, admin approvals (before 2026-11-15)

Works against today's member vault: the service is an ordinary `connection`, sign-in uses `connauth`.

| ID | Work item | Repo | Done when |
|---|---|---|---|
| 1.1 | `internal/peer`: relay mailbox, suite-2 handshake, connection tokens, `connection.removed`, on vettid-vault `vms/*` + `relayclient` | service-vault | Completes a connection with a real member vault on staging |
| 1.2 | Keystore (KMS envelope, persisted) and DynamoDB store; identity and pending challenges survive restart | service-vault | Restart keeps `service_id` and in-flight sign-ins |
| 1.3 | Connect: invite API, acceptance webhook, email bound as HMAC + pepper | service-vault | A phone connects from a link or QR; webhook fires |
| 1.4 | Sign-in: uniform-response lookup, rate limits, `connauth` challenge with match code in `context`, 5-minute `exp` | service-vault | Lookup-then-approve works end to end |
| 1.5 | Webhooks (HMAC over timestamp.body), API keys, audit log | service-vault | Tests cover signing, replay and auth |
| 1.6 | Admin approvals: action hash in `connauth` `context`, configurable m-of-n, audited | service-vault | 1-of-n enforced; threshold changes by config only |
| 1.7 | Go SDK: invite, lookup, sign-in, approvals, webhook verify | service-vault | Member API can be written against it |
| 1.8 | CDK per §9 | service-vault | `cdk deploy` serves at the §9 cost |
| 1.9 | Android: show the match code from `context` on the authentication approval (verify current behaviour first) | vettid-android | Code visible to the member |

Gate: service vault deployed; a phone connects from a link, signs in by email, and a governance action requires an admin's approval.

### Phase 2 — Member service core (~2.5 weeks)

| ID | Work item | Repo | Done when |
|---|---|---|---|
| 2.1 | Static site: connect page (email + link/QR), email sign-in page, member home | member-service | Served from CloudFront via StaticSite |
| 2.2 | Member API: session via SDK, webhook receiver, roster | member-service | §4.1 and §4.2 work against the deployed service vault |
| 2.3 | CDK for member-service (web, api, tables) | member-service | One deploy |
| 2.4 | iOS: match code on the authentication approval | vettid-ios | iOS completes §4.2 (push when APNs lands) |

Gate: a provisional member connects, signs out, signs back in by email.

### Phase 3 — Service contracts in the member vault (~3 weeks)

| ID | Work item | Repo | Done when |
|---|---|---|---|
| 3.1 | Spec: service connections and contracts — service kind or contract in the invite bundle, canonical encoding, signed acceptance, sever, `completion.sign`, wire vectors | vettid.org | Spec approved |
| 3.2 | Contract acceptance, storage, sever; `completion.sign` | vettid-vault | Vectors pass |
| 3.3 | Contract review screen; completion approval | vettid-android, vettid-ios | Both apps accept a contract and sign a completion |
| 3.4 | Contract offers and enforcement; completion verification; `admin-v1` replaces connauth-based approvals | service-vault | Requests outside the contract are refused |

Gate: a member accepts `member-v1` and signs a completion.

### Phase 4 — Beta program (~2 weeks)

| ID | Work item | Repo | Done when |
|---|---|---|---|
| 4.1 | Cohorts and invitations | member-service | Cohort 2 invited through the service |
| 4.2 | Assignments: task cards delivered as feed events, signed completions, feedback forms | member-service, service-vault | Completion appears in the dashboard from a signed event |
| 4.3 | Admin dashboard: completion rate, active members, time-to-complete | member-service | Numbers derived from signed completions only |
| 4.4 | RUNBOOK.md: cohort operations | member-service | Cohort 2 run from the runbook |

Gate: cohort 2's first assignment is "connect to the member service."

### Phase 5 — Proposals and voting (~4 weeks)

| ID | Work item | Repo | Done when |
|---|---|---|---|
| 5.0 | BALLOT-PROTOCOL.md: tags, ballots, receipts, results, publication and anchoring, verification, limits | service-vault | Reviewed before ballot code starts |
| 5.1 | VAULT-MESSAGING: vote messages and vectors | vettid.org | Spec approved |
| 5.2 | votes.vettid.org: write-once publication with an upload API that checks signatures and Rekor anchors | votes | A proposal and its results publish and verify |
| 5.3 | Anchor key (P-256, envelope-encrypted) and Rekor anchoring | service-vault | A document anchors and its bundle verifies |
| 5.4 | `ballot`: proposal signing, tags, cast handling with receipts, results, publication | service-vault | Non-binding test vote verifies end to end |
| 5.5 | Vote feature: per-proposal key, ballot and credential signatures, receipt, own-row check, recovery | vettid-vault | Vectors pass; cast and verify on staging |
| 5.6 | Vote and verify-my-vote UI | vettid-android, vettid-ios | Both apps cast and verify |
| 5.7 | Member API/site: proposal lifecycle, results | member-service | First provisional vote runs as a cohort-2 assignment |
| 5.8 | Ballot code review (peer or commissioned) | — | Sign-off before the first binding vote |

Gate: a non-binding vote published, verified by a member and by an outside observer.

### Phase 6 — Comments and notifications (~1 week)

| ID | Work item | Repo | Done when |
|---|---|---|---|
| 6.1 | Threaded comments on proposals and assignments | member-service | Members-only; cohort-scoped on assignments |
| 6.2 | Notification spec for service events (feed kind, priority, channel); `urgent` on governance deadlines | vettid.org, member-service, service-vault | Delivered per spec on both apps |

### Phase 7 — Optional, later

An optional secret-ballot mode, if one is ever needed; billing-backed membership once payments and a service status route exist; Node and Python SDKs; Discourse with VettID SSO as a third-party auth-to-service demo.

**Total:** ~16–18 weeks solo, plus iOS catch-up and review wait time.

## 11. Open questions

1. **Operating agreement** — quorum, record date, member of record for binding votes; counsel before the first binding vote.
2. **iOS push** — APNs setup (IOS-PLAN phase P) gates iOS sign-in by push.

Decided and closed: service vault model; no enclave; service vault rebuilt from scratch in a new `service-vault` repo, no prototype code kept; member-vault support for service connections, contracts and votes (VAULT-MESSAGING scope lifted); email entered on the site and stored as an HMAC; connect link on member.vettid.org; email-initiated sign-in over `connauth`; `urgent` for deadlines; provisional→member on paid subscription; admin approvals from day one, 1-of-n in v1 with a configurable threshold; shared relay client is `vettid-relay/relayclient`; pseudonymous ballots over the member's connection, published on votes.vettid.org (2026-10-29); voting window ≤ 7 days; 6-month feedback retention; Option D for discussion; no re-voting; Phase 1 before 2026-11-15.

## Changelog

- **0.7.0** — Voting is pseudonymous rather than secret (owner decision 2026-10-29): §8 rewritten to BALLOT-PROTOCOL 0.10.0; blind signatures, the ballot mailbox and the token pool are gone; votes.vettid.org publishes proposals and results, anchored in Rekor. Phase 5 rows and §2 updated. Members are told before voting that the organisation sees their vote.
- **0.6.4** — Suspended connections: the service vault's `connection.suspended` webhook; the roster marks them for admins and doesn't count them, and their sessions end (§6).
- **0.6.3** — Changed-key sign-ins (owner decision 2026-10-27): an approval with `key_changed` needs an emailed code before a session, and alerts (§4.2 step 6).
- **0.6.2** — Roster visibility (owner decision 2026-10-27): members see the member count; admins also see names and join dates (§6).
- **0.6.1** — Sign-in challenge `exp` five minutes, not two (owner decision 2026-10-27): on the first staging run, two sign-ins expired while the member found the request on the phone and entered the credential password. VAULT-MESSAGING §10.4 allows up to ten (§4.2, 1.4).
- **0.6.0** — Rebased on the state of the code on 2026-10-26. Service vault rebuilt from scratch as a VMS v2 peer in a new `service-vault` repo; prototype retired. Member-vault work added (service connections, contracts, completions, votes) and VAULT-MESSAGING scope lifted. Email captured on the site, stored as HMAC. Sign-in and interim admin approvals over `connauth`. Services reach phones through the member's vault. Ballot: vote root across credential rotation, `SHA256(proposal_id)` salt, RFC 9474 via circl, timestamp reduction in the ballot box, 409 rule, pool as static file, ≤ 7-day window, operator-metadata boundary. Contracts become Phase 3; total ~16–18 weeks.
- **0.5.2** — `relay-go` dropped: `vettid-relay/relayclient` is the shared Go client. Ballot deposits redesigned on the existing protocol's one-shot open tokens (§5.6) — public pool plus ephemeral sender key — so no relay protocol change is needed (§7.1, §8, 4.1). Phase 1 shortened; total ~8–9 weeks. One open question remains.
- **0.5.1** — Admin threshold 1-of-n for v1; relay ballot requirement stated.
- **0.5.0** — Restructured as a repo-organization and implementation plan; §3 redrawn as three parties; connect-link onboarding; repo layouts; work items with done-when criteria.
- **0.4.0** — Rebuilt on the existing `vettid-service-vault` repo; no enclave; cost revised.
- **0.3.0** — Decisions from 2026-10-26 review; vettid.dev voting code reviewed.
- **0.2.0** — Secret ballot in v1.
- **0.1.1** — Diagram fix.
- **0.1.0** — Initial draft.
