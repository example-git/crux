# Remote workspaces and pairing

A saved Crux connection lets your local client select providers, accounts and
models while a remote server runs the agent and tools. In a client-owned
workspace, the server receives the selected declarative provider/preset bundles,
required assets and credential bindings over mutually authenticated TLS. Those
bundles do not need to be installed on the server.

The execution server necessarily receives usable credentials. This is a
single-owner workflow between trusted machines, not a provider relay that keeps
credentials entirely on the client.

## Pair a client

On the server:

```sh
crux server setup \
  --host tcp://0.0.0.0:9090 \
  --advertise tcp://server.example:9090 \
  --workspace-root /srv/projects
```

Setup prints a `crux connections pair NAME SETUP_CODE` command for the client.
Use a reachable advertised host and port; a wildcard listen address cannot be
advertised. The default enrollment lifetime is ten minutes, configurable with
`--enrollment-ttl`. Enrollment uses a separate temporary listener, not a route
on the normal daemon API.

The client verifies the pinned server certificate and saves a private pending
identity before submitting its public certificate. On the server terminal,
review the candidate name, client fingerprint, server fingerprint, endpoint and
expiry. Enter `y` or `yes` to approve that exact candidate. A setup code alone
does not bypass this local approval. Denial, cancellation, expiry or exhausted
failure budgets prevent a later authorization commit.

After authorization, the client promotes the retained identity to its saved
connection and checks that the same pinned server currently authorizes it. On
Linux, setup normally installs and starts the user service. `--foreground` runs
in the current terminal; unsupported service-management platforms also use the
foreground server.

If authorization succeeded but service installation failed, retain the saved or
pending identity. The setup error includes the daemon-install recovery command.
Starting the daemon and recovering the same identity does not require a second
enrollment.

### Recover an interrupted pairing

On the client:

```sh
crux connections pending
crux connections recover OPERATION_ID
```

The pending list shows operation IDs and public identity metadata, never private
keys or setup tokens. Recovery proves that the **retained client certificate**
is currently authorized by the **same pinned server** before saving it. A health
response is not sufficient proof.

If the intended local name is already occupied, keep both records and choose an
unused alternate name explicitly:

```sh
crux connections recover OPERATION_ID ALTERNATE_NAME
```

Pending identities are not automatically discarded when the enrollment code
expires. A lost response, canceled wait or local save error can follow a
successful server authorization, so do not start over with another key merely
because the initial command failed.

To deliberately abandon the retained key:

```sh
crux connections forget-pending OPERATION_ID --confirm-key-loss
```

This deletes the pending **local** identity. It does not revoke any server grant.
Arrange server-side revocation separately when that identity may be authorized.
If the connection was already saved and only pending-record cleanup failed, the
error reports that saved state distinctly.

### Manual pairing

Manual public-code exchange remains available:

1. Run `crux connections server-init` on the server and transfer its public code.
2. Run `crux connections add NAME tcp://HOST:PORT SERVER_CODE` on the client.
3. Transfer the resulting public client code to the server and run
   `crux connections authorize NAME CLIENT_CODE`.
4. Review and approve the exact fingerprints at the server terminal.
5. Start `crux server --host tcp://0.0.0.0:PORT --workspace-root /srv/projects`.

Private client and server identity keys stay on the machine that creates them.
Losing a client key requires a newly approved identity and explicit revocation
of the old principal. Replacing the server identity invalidates existing pins
and requires an explicit new pairing. Provider/account backups and pairing
identity backups serve different purposes.

## Connect and select authority

```sh
crux --connection NAME
```

Without an explicit workspace flag, the client opens the remote workspace menu.
It lists the authenticated principal's workspaces and provides a bounded
directory browser. `--cwd`, `--data-dir`, `--session`, `--continue`, `--yolo` or
`--channels` selects the direct workspace flow instead.

New workspaces created through a saved connection use client-owned provider
authority. The local working directory supplies the client configuration and
selected bundles; the remote working directory is where tools execute. The
server's installed provider with the same ID does not replace the client's
definition.

Server-owned mode remains available for local/server-managed workspaces and as
an explicit `authority_mode: "server"` API creation choice. Opening an existing
server-owned workspace preserves that mode. There is no automatic transition
from a missing client provider or expired credential to server providers,
accounts or models.

The compact chat header shows the accepted provider source and client runtime
revision. The Workspace Authority sidebar section shows the client fingerprint
and the selected saved account ID when one is part of the accepted runtime.
Authentication dialogs provide exact provider-owner and credential choices.
These views contain accepted metadata, not usable credentials.

### Negotiation and selected inputs

The authenticated client first calls `GET /v1/runtime-capabilities`. It checks
the protocol, compiler and advertised bounds before constructing or sending a
private proposal. Incompatible peers fail explicitly; the client does not retry
with legacy forwarding or silently request server-owned mode.

Collection includes selected main/auxiliary providers, configured image
dependencies and the selected account for each required provider. It does not
forward every saved account. Bundle bytes and assets come from the captured
configuration scan. Runtime admission rejects malformed or unsupported bundles,
bad digests, unsafe paths, oversized input and mismatched credential owners or
destinations before publication.

The complete process environment is not forwarded. Required resolved values and
explicitly declared credential environment inputs may be included in the
private snapshot. Client-owned image plugins can carry cookies from the exact
selected browser profile and declared domains, and declared client identity
values. The receiver does not select another browser profile or probe its own
machine for a replacement client identity.

## Updates, authentication and refresh

Provider, model, account and runtime-control changes in a client-owned workspace
persist on the owning client and submit a complete next runtime revision. The
server stages and validates it before an atomic replacement. Requests already
running retain their captured authority; later requests use the acknowledged
revision.

A local save and a remote publication are separate outcomes. An error can mean
that a credential was saved but publication was not acknowledged. Preserve the
operation identity and inspect the reported account/configuration/publication
progress. Retrying the original request, recovering its publication, and
explicitly reviewing currently saved state are different actions.

The provider credential flow checks an exact owner and credential field, retains
the checked input, and saves that same input without repeating resolution or a
probe. Manifest-declared configuration credentials appear as separate fields
alongside the primary API key and OAuth choice. Saving one field preserves the
other credential fields, accounts and model selection.

A property field uses an applicable declared catalog probe. When no such probe
is declared for that field, Check explicitly reports source/schema validation
only. This does not establish that the credential can authenticate. Partial
setup also reports no probe and lists the remaining required inputs; a saved
partial configuration remains unavailable for execution until those inputs are
complete. Malformed or ambiguous probe declarations fail visibly.

OAuth interaction completion is followed by the persistence/publication transaction;
an authorization callback alone does not mean the workspace is using the new
account. Changing an authentication credential does not select a different model.

Account commands use the selected workspace's authority:

```sh
crux --connection NAME --cwd /srv/projects/PROJECT accounts list
crux --connection NAME --cwd /srv/projects/PROJECT accounts switch PROVIDER ACCOUNT_ID
crux --connection NAME --cwd /srv/projects/PROJECT accounts remove PROVIDER ACCOUNT_ID
crux --connection NAME --cwd /srv/projects/PROJECT accounts logout PROVIDER
```

Removing an inactive account does not need a model or runtime change. Removing
the active account also clears its selected configuration and publishes the
result. Logout clears the provider credential and its saved accounts. A
concurrent replacement or selection change invalidates an old operation rather
than authorizing it to affect the new selection.

Automatic OAuth refresh runs on the owning client. The receiver requests refresh
for an exact principal, runtime, provider definition, account and credential
generation. The client saves the rotated token before publishing and
acknowledging the replacement. Completed rotation receipts let other workspaces
adopt the proven result without consuming the same refresh token again.

Image HTTP authentication recovery retries the failed request boundary after an
acknowledged refresh. It does not restart earlier workflow steps or repeat an
entire completed image workflow. Declared request, origin and retry budgets
continue to apply.

## Disconnects, storage and history

A workspace belongs to one authenticated certificate. Knowing another workspace
ID or using the same client UUID does not confer ownership. One active authority
owns a canonical workspace path; different certificates do not implicitly share
the workspace or its history through workspace APIs.

The default disconnect grace is ten seconds; the server advertises its actual
configured value during negotiation. Accepted foreground and detached work may
continue during that grace. When the final claim expires or the workspace is
explicitly retired, credential-bearing work is canceled and drained. Detaching
a task does not grant indefinite access to an abandoned runtime.

Live reattachment uses the same principal and accepted runtime authority.
Workspace loss or daemon restart requires a newly negotiated proposal from the
client. Public discovery and server configuration are not substitutes for the
client's private inputs.

Client-owned data directories are scoped to the certificate and, for an explicit
data root, the canonical workspace path. Reopening the same directory can make
saved sessions available, but path reuse is not proof that history was loaded or
that a prior job resumed. Credentials are not restored from history, and
credential-bearing background work is not automatically resumed after restart.

## Inspect and revoke authorization

Run authorization administration on the server:

```sh
crux connections authorized
crux connections revoke NAME
```

The listing shows public fingerprints, authorization status and known creation,
approval, last-use and revocation dates. Unknown historical dates remain unknown;
certificate validity dates are not used as invented activity history.

Revocation first saves an exact operation for the principal and grant. Registered
running daemons then cancel and join the affected requests, streams, workspaces,
agents, image jobs and other credential work. This includes removing the final
remote principal: acknowledgement uses a private local control channel rather
than the removed remote grant. A successful drain does not require a daemon
restart or reconstruction of its TLS configuration.

Read the outcome precisely:

| Reported state | Meaning |
| --- | --- |
| Stored authorization revoked | The durable grant was removed. |
| All registered daemons acknowledged | Those daemons confirmed cancellation and actual drain for the exact principal/grant. |
| Saved, but live cancellation not fully acknowledged | At least one daemon is unavailable, stale, timed out or has not completed cleanup. |
| No live daemon registered | No live cancellation acknowledgement was received. |

Retry an incomplete acknowledgement with the **same** operation ID printed by
the original command:

```sh
crux connections revoke NAME --operation OPERATION_ID
```

That retry does not remove a later replacement grant. Cleanup may continue after
the administrative wait times out. Noninteractive initial revocation requires
`--force`; it does not change what counts as a drain acknowledgement.

## Scope and operational limits

The normal provider workflow does not install client bundles or save client
credentials into server provider/account/configuration stores. Workspace
databases and task/session metadata are separate durable state. Pairing and
authorization administration intentionally update their private connection and
authorization records; live usage tracking can update authorization last-use
metadata.

Forwarded credentials and private bundle contents are excluded from public
workspace discovery and private-request traffic bodies. Secret redaction keeps
process-keyed fingerprints for delayed logs without retaining plaintext secret
registry entries. This is not a guarantee of physical memory zeroization; the
executing process, a debugger or a process dump can expose live credentials.

Enrollment bounds concurrent connections/requests and applies rate and failure
budgets. Five invalid-token attempts can still close an enrollment window, so
someone who can reach it can deny that short setup window. These safeguards are
not a network-flood defense.

The baseline uses explicit local approval, not signed authorization-list
administration. It does not require separate OS accounts and does not claim to
contain hostile code with unrestricted access as the daemon user. That code may
access the same filesystem, process or trust configuration. Use stronger OS
isolation when that is part of the deployment's requirements.

## API and schema references

The daemon serves its generated API documentation under `/v1/docs/` behind the
same authorization boundary as other routes. The remote runtime schema is
generated by `crux schema remote-runtime`. It describes private proposal fields;
structural schema validation alone does not replace capability negotiation,
digest checks, exact owner binding, destination policy or runtime admission.

See [provider bundle documentation](provider-plugins/README.md) for declarative
credentials, OAuth, instructions, operations, usage and images.
