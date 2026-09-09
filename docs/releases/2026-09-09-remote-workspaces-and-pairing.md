# Remote workspaces and pairing — unreleased integration, 2026-09-09

This integration lets a saved Crux client connection use its own selected providers,
accounts, models, configuration and plugin assets on a remote execution server. The
server does not need those provider or preset bundles installed. Pairing now requires
explicit approval on the server, retains interrupted client identities for recovery,
and supports revoking a client on the running daemon with an explicit drain outcome.

Implementation range: 227 commits from `16b323b3ca3fd2d1b8b8612ce9f3b55b4c13903e`
through `6adc6a574b432bb6579991552e6d08c2e3a415e1`. These are integration notes; they do not
assign a release version or announce a published release.

## Client-selected remote provider execution

- New workspaces opened through a saved connection use client-owned provider
  authority. The client working directory supplies the selected configuration and
  bundles; the remote working directory is where tools execute.
- A private runtime proposal carries selected main and auxiliary providers, image
  dependencies, exact model choices, runtime controls, instructions, required assets
  and each selected provider's credential binding. It forwards the chosen account,
  where applicable, rather than exporting every saved account.
- The receiver compiles an independent workspace runtime. A server-installed provider
  with the same ID cannot replace the accepted client definition, credentials, model,
  headers or branding. Client bundles are not installed into the server's plugin store
  or published into its global registry.
- Provider, model, account, instruction and runtime-control changes submit a complete
  next revision. Validation and staging finish before atomic publication. Invalid or
  stale updates preserve the previously accepted runtime; in-flight requests retain
  their captured inputs.
- Missing client providers, incomplete credentials and incompatible runtime proposals
  fail explicitly. Existing server-owned workspaces retain their mode, and API callers
  can explicitly request `authority_mode: "server"` for a new workspace. Client-owned
  execution does not silently switch to server providers or accounts.
- The chat header and Workspace Authority sidebar show accepted provider source,
  runtime revision, client fingerprint and selected account identity where present.
  These are public authority metadata and contain no usable credentials.
- `crux --connection NAME` opens the authenticated workspace menu and bounded remote
  directory browser unless direct workspace options select the direct flow. Reusing
  a path or reconnecting is reported separately from loading a saved session.

## Runtime negotiation, API and schema changes

Clients authenticate and call `GET /v1/runtime-capabilities` before constructing or
sending a private runtime proposal. Both peers must support runtime protocol version
1 and the exact compiler identity `crux-declarative-runtime-v23`. Upgrade both sides
to a compatible build before using client-owned remote workspaces. An older receiver
is rejected explicitly, without legacy forwarding or server-owned fallback.

The proposal limit is 96 MiB, with at most 64 bundles and 64 provider definitions.
Admission checks exact bundle bytes and digests, strict schemas, bounded assets,
duplicate ownership, unsafe paths, credential owners and destinations before
publication. Structural JSON Schema validation alone is not runtime admission.

The typed API now represents accepted authority, runtime revisions and
acknowledgements; provider authentication status and transactions; OAuth sessions,
reload, review, repair and retained history; normalized provider usage; and pinned
client authorization proof. Private proposal fields remain absent from public
Workspace discovery and acknowledgements.

`CurrentSession.selection_generation` orders session selection within a client UUID
and workspace so a delayed selection cannot replace a newer intent. An explicitly
supplied zero is invalid. Omission permits legacy updates until a generation has been
accepted for that client and workspace; later unversioned updates conflict. Compiler
v23 negotiation ensures a client-owned receiver implements the ordering contract.

`crux schema remote-runtime` generates the private runtime schema. The Taskfile, schema
update workflow and release generation include it with the existing configuration,
provider, preset, image-provider and branding schemas. The regenerated OpenAPI contains 94 paths and
201 definitions and is served under the authenticated `/v1/docs/` route. The final
source commit adds the current-session generation field to its Go, JSON and YAML
artifacts. Provider manifest version 1 remains the declarative bundle contract.

## Authentication, account management and recovery

Authentication actions now retain the exact provider owner, credential field, account,
configuration generation and workspace incarnation that admitted them. Saving locally
and publishing remotely have separate receipts. A lost response no longer requires
starting a new login or replacing the original account choice.

- API-key Check retains the exact checked input, and Save uses that retained input
  without resolving it again or repeating the probe. Declared configuration credentials
  are separate selectable fields alongside a primary key or OAuth. Saving one field
  preserves other fields, accounts and model selection.
- Check reports the evidence it actually obtained. An applicable declared catalog probe
  can validate authentication; a field without one reports source/schema validation
  only. Partial setup lists remaining required inputs and stays unavailable for
  execution until complete. Malformed or ambiguous probes fail visibly.
- OAuth code, paste, fixed/dynamic callback and device flows use owner-bound sessions.
  Callback completion is followed by the durable save and publication transaction;
  completing browser authorization alone does not mean the workspace adopted the new
  account. Authentication changes preserve the selected model.
- Retained OAuth results can be recovered from the original operation without another
  token exchange, subject to the original owner and captured-input checks. A tokenless
  preparation or unknown exchange outcome has an explicit abandonment action. An
  unknown response is not relabeled as a successful login or retried as a new exchange.
- Account listing, switching, removal, logout and supported import paths use the selected
  workspace's authority. Removing an inactive account preserves runtime selection.
  Removing the active account selects the first remaining account in stored order;
  removing the last clears the credential. Logout clears that provider's credential
  and saved accounts. Concurrent replacement invalidates stale actions.

**Review Saved Authentication** (`review_saved_authentication`, `saved_auth`, or
`reload_auth`) separates reading current status, explicitly reloading owner-local files,
reviewing an exact saved choice, and publishing the retained preview. Reload can
re-evaluate configured sources and model discovery, but does not itself publish a
runtime. Retries reconcile the original request rather than silently collecting a new
choice. A later successful review does not rewrite an older operation's outcome.

**Authentication History** (`auth_history`) and `accounts history [--json]` expose
redacted progress for original operations and reviews, including local writes,
receiver acknowledgement and local adoption. Historical workspace incarnations remain
attached to their original targets and cannot publish into a recreated workspace.
Closing a dialog preserves admitted operations and eventual receipts.

`accounts repair-local` previews and applies the exact recorded account/configuration
postimages using the reviewed journal revision. It refuses conflicting newer state,
does not repeat token exchange, and does not publish remotely. Follow repair with an
explicit reload and review when publication is wanted. Publication retirement, review
retirement and local recovery abandonment preserve the original known or unknown
outcome instead of claiming to undo earlier writes or acknowledgements.

`accounts pending-oauth` and `accounts retire-oauth` can inspect and retire orphaned
operations using explicit original local configuration paths, even when the provider
was removed or configuration no longer loads. They do not initialize a provider or
connect to a receiver. Discarding a recorded token is an additional explicit choice;
it retires recovery and does not revoke that token at its provider.

Each private authentication journal is bounded to 128 records and 256 MiB, including
reserved completion space. Completed or explicitly retired records are pruned oldest
first when needed. Unresolved operations require recovery or explicit retirement;
capacity refusal never justifies repeating a possibly completed token exchange.

## Refresh and all credential-bearing consumers

Automatic refresh runs on the owning client. A request is bound to the exact principal,
provider definition, account or configuration-only token, and credential generation.
The client durably saves a rotated token before submitting the replacement runtime and
checking its exact acknowledgement. Other workspaces can adopt a proven completed
rotation without spending the same refresh token again. Removed or replaced accounts,
logout and changed configuration cannot be revived by delayed refresh completion.

The accepted runtime now reaches main inference, title and memory requests, suggestions,
summaries and compaction, instructions, runtime controls, provider usage, image jobs,
account actions and reachable diagnostics. Captured authority also scopes continuation
and socket state. A retry retains the selected owner and operation instead of changing
provider or replaying an ambiguous accepted operation through another path.

Client-owned image execution can carry cookies from the exact selected browser profile
and declared domains, plus explicitly declared client identity values. It does not
choose a different profile or probe the server for a replacement identity. Image OAuth
recovery retries the failed HTTP boundary after an acknowledged refresh, preserving
completed workflow steps and declared request/retry budgets. Quota responses are
normalized under the accepted provider owner.

Provider and MCP destination checks apply at dispatch as well as admission. Declared
provider destination and redirect policies remain effective; native/custom credential
redirects remain bound to the selected endpoint origin. MCP resource traffic, including
SSE message POSTs, is bound to the configured MCP origin. OAuth discovery may legitimately
select separate registration and token servers, each bound to its selected origin.
Public metadata repair cannot move credentials or token request bodies elsewhere.

A nonretryable destination refusal survives SDK protocol negotiation and cannot trigger
another automatic authentication attempt or clear a saved OAuth token. Genuine
authentication failures retain their intended recovery path. Rejected request bodies
are closed, and MCP connections and processes follow the workspace lifetime.

## Pairing and interrupted setup

`crux server setup` uses a separate temporary enrollment listener, with a ten-minute
default lifetime and a configurable `--enrollment-ttl`. A wildcard listen address needs
a reachable advertised address. The normal daemon API does not become an enrollment
endpoint. Setup codes use enrollment protocol version 2; mismatched versions are
rejected explicitly.

Before authorization commits, the server terminal displays the exact candidate name,
client fingerprint, pinned server fingerprint, endpoint and expiry and requires local
`y`/`yes` approval. Manual `connections authorize` also reviews the exact identity.
Possession of the setup code alone is insufficient. Denial, cancellation, expiry,
listener closure and exhausted failure budgets are fenced against late authorization.

After verifying the server pin, the client durably stages its exact private pending
identity before submitting authorization. Lost replies, interrupted waits and local
save failures retain the key and operation. `connections pending` lists public metadata;
`connections recover OPERATION_ID` proves that same retained certificate is currently
authorized by the same pinned server before promotion. Generic health is insufficient.

The private pending-identity store is bounded to 64 operations and 4 MiB. Capacity
requires recovery or explicit local key abandonment before another enrollment.
A name conflict preserves both identities and requires an explicit alternate local name.
Pending identities remain inspectable after enrollment or certificate expiry; expired
certificates still cannot authenticate or be promoted. `connections forget-pending OPERATION_ID --confirm-key-loss` deliberately removes only that local pending key and
does not revoke a server grant. A saved connection with failed pending cleanup is
reported distinctly. If server service installation fails after authorization, the
reported daemon recovery command and retained identity avoid a second enrollment.

Enrollment has separate bounded admission and failure controls:

| Control | Limit |
| --- | --- |
| Open connections, including TLS handshakes | 8 |
| Connection admission | Burst 16; refill 4 per second |
| Concurrent admitted requests | 4 |
| Request admission | Burst 8; refill 2 per second |
| Invalid setup-token attempts | 5 closes the window |
| Malformed valid-token submissions | 5 closes the window |
| Admitted authorization failures | 3 closes the window |

Rate/concurrency refusals do not consume those failure counters. These are bounded
setup safeguards: someone able to reach enrollment can still exhaust its short window;
this is not network-flood protection.

## Principal ownership, disconnects and live revocation

A verified client certificate owns each workspace. A caller UUID, known workspace ID or
matching path does not grant another certificate access. One active authority owns a
canonical path. Client-owned databases, saved sessions and memory are certificate-scoped,
with canonical workspace identity also included when an explicit data root is used.
Prompt loading, relevant-memory retrieval and memory tools use those roots rather than
the daemon user's global memory topics.

The default final-disconnect grace is ten seconds, and negotiation reports the
server's actual setting. Foreground and detached work can continue within that grace.
When the final claim expires or the workspace is explicitly retired, cleanup cancels
and joins credential-bearing work, including auxiliary/title/summary calls and images.
Detached work does not gain indefinite access to an abandoned runtime.

Reattachment requires the same certificate and exact accepted authority. Daemon restart
or workspace loss requires a newly negotiated client proposal. Saved history does not
restore credentials or automatically resume credential-bearing background work.
Current-session generations and workspace-incarnation checks keep delayed UI replies
and old session intentions from replacing current state.

`connections authorized` separates persisted public grant history from volatile live
last-use observations reported by registered daemons. Live observations remain in
memory and do not rewrite authorization files. Unknown dates remain unknown.

`connections revoke NAME` records the exact principal/grant operation, then asks running
daemons to cancel and join affected requests, streams, workspaces, agents and image
jobs. This works for the final remote principal through a private local control channel
without restarting the daemon or rebuilding its TLS configuration. Fresh and reused
connections are denied after revocation while retained principals continue to work.

The result distinguishes durable revocation from acknowledged live drain, unavailable
or incomplete daemon responses, and no live daemon registration. An incomplete result
can be retried with the same `--operation` ID without revoking a later replacement grant.
`connections revocations`, `abandon-revocation --confirm-unacknowledged`, and `audit-prune`
make historical unknown outcomes explicit and preserve unresolved work.

New grants reserve capacity for their later revocation. A combined limit of 256 covers
active grants and unresolved revocations, with up to 256 completed/abandoned revocation
receipts and 256 inactive authorization summaries. Legacy necessary state can exceed those growth
bounds without having grants or unresolved receipts silently discarded.

## Data handling and supported trust boundary

Ordinary client-owned provider work does not write client bundles or credentials into
server provider, account, configuration or plugin stores, or rewrite server trust and
daemon records. Workspace databases and session/task metadata are separate durable
state. Explicit pairing and authorization administration update their own records;
daemon startup and exit update registration.

Forwarded credentials and private bundle contents are excluded from public workspace
discovery and private-request body logging. Delayed redaction uses process-keyed
fingerprints instead of retaining plaintext secret registry entries.

The remote execution server necessarily receives usable credentials. This is a trusted
single-owner machine-to-machine execution model. It does not provide hostile code
containment for another process running unrestricted as the daemon user, signed
administrative authorization lists, privileged-file rollback protection, or physical
credential memory zeroization. Pairing key loss requires a newly approved identity and
explicit old-principal revocation; replacing a server identity requires explicit new
pinning/pairing. Provider/account backups and pairing identity backups are distinct.

## Validation and evidence

The completed project records terminal evidence for **52 changed Go directories**:
**50 with tests and two without test files**. Config, Workspace and UI/model use their
completed full-suite complements plus corrected targeted passes. This is not a claim
of one clean full-repository test command at the final commit. Failed attempts remain
retained with their corrections, and no required test tail remains unexecuted.

Recorded automated coverage includes real TLS/HTTPS authentication and account flows,
pairing and recovery, concurrent refresh, principal isolation and memory, actual
credential-work cancellation, provider/MCP destinations, images, normalized usage,
compaction, UI interaction and lifecycle behavior. All three 128-operation Workspace
durability/capacity workloads completed. The final root-package race run passed.
The validator also read **32 local plugin/preset fixtures**—six user plugins and
26 permitted presets—without modifying or treating them as external service tests.

The six schema targets and three OpenAPI outputs reproduced byte-for-byte across two
passes; local JSON references resolved. The final native binary was rebuilt after the
three reviewed generated OpenAPI files were committed. Windows AMD64 application and
connection/config/providerauth/cmd test binaries compiled and their PE signatures were
inspected. **Windows validation is compile-only.** Its initial post-build guard rejected
changed root Finder metadata; retained input and binary reconciliation verifies the
outputs and records that initial runner failure rather than calling it a clean exit.

Three standalone acceptance scenarios ran separate disposable CLI daemons using the
exact final native binary at `6adc6a57`:

| Scenario | Observed result |
| --- | --- |
| Client runtime and revocation | Mutual-TLS admission, client-only preset main/title/memory requests despite an enabled same-ID server definition, unchanged protected server stores, exact-authority reconnect, acknowledged active HTTP drain, fresh/reused revoked-client denial, and retained-principal inference on the same daemon. |
| Images and quota | A real model-to-imagegen tool job produced the exact expected 2×2 PNG; client-only quota execution normalized a remaining fraction of 0.25 to 75% weekly utilization; exact owner/credentials and unchanged protected server stores were verified. |
| Refresh and compaction | One real HTTPS token exchange, durable selected-account and application-data writes, fresh ConfigStore reconstruction without another exchange, exact revision-2 acknowledgement, resumed inference, one provider-backed local-summary request, a saved checkpoint and subsequent checkpoint replay. |

All three observed zero foreign requests. These scenarios used synthetic loopback
providers and credentials, with mutual TLS between client and daemon. They do not
establish live compatibility with every external provider. The standalone compaction
case exercises the declared `local-summary` policy; other alternatives retain their
separate automated evidence. Each daemon used fresh state, explicitly initialized its
bundled tool before the protected-store baseline, and exited through cleanup.

The final evidence index records the source and binary hashes, artifact reproduction,
Windows reconciliation, package inventory, all three live result/manifests, and the
completed nine-boundary authority review. The native binary SHA-256 is
`5d8f8192041f721224da9d5798771d10c637c0b842d490ef7e03076242fea073`.

For operational commands and precise recovery semantics, see
[Remote workspaces and pairing](../remote-workspaces.md). For the declarative provider,
credential, operation, usage and image contracts, see
[Provider plugin manifest v1](../provider-plugins/README.md).
