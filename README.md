# Crux

Crux is a terminal-first agentic coding harness and an independently maintained derivative of [Charmbracelet's Crush](https://github.com/charmbracelet/crush).

It provides an interactive coding session, repository tools, language-server integration, MCP support, permissions, hooks, skills, and a provider system built around strict declarative schemas.

> Crux is not affiliated with, sponsored by, endorsed by, or maintained by Charmbracelet, Inc.

## Provider system

Crux treats provider integration as a host-controlled protocol boundary. Provider bundles describe configuration and capabilities as data; they do not load executable plugin code.

Two bundle types are supported:

- **Provider plugins** declare one provider, its models, configuration schema, authentication requirements, compatibility range, and the host operations it needs.
- **Provider presets** supply catalog metadata for an implementation already owned by the Crux Foundation runtime. They do not register or add runtime implementations.

The host retains control of credentials, HTTP and WebSocket transports, OAuth, request construction, response handling, persistence, and tool execution.

### Data-only bundles

Provider bundles cannot add Go code, WebAssembly, subprocesses, daemons, shell expansion, or arbitrary network behavior. Unknown ordinary fields fail validation, and unsupported required features make a bundle incompatible.

Installation is explicit. Crux validates the source, copies it into a host-owned immutable snapshot, and identifies it by a canonical SHA-256 digest. Trust applies only to those exact bytes. A changed, invalid, incompatible, revoked, or untrusted bundle remains inactive.

Git sources must use HTTPS and are never updated automatically.

### Schemas

The checked-in schemas are the canonical contracts for configuration and provider bundles:

- [Crux configuration schema](schema.json)
- [Provider plugin schema](provider-plugin.schema.json)
- [Provider preset schema](provider-preset-plugin.schema.json)

Raw repository URLs can be used by editors and bundle manifests:

```text
https://raw.githubusercontent.com/example-git/crux/main/schema.json
https://raw.githubusercontent.com/example-git/crux/main/provider-plugin.schema.json
https://raw.githubusercontent.com/example-git/crux/main/provider-preset-plugin.schema.json
```

The provider schemas use JSON Schema Draft 2020-12. Bundle compatibility is negotiated through explicit host API versions, host release constraints, and required or optional feature identifiers.

See [`docs/provider-plugins/README.md`](docs/provider-plugins/README.md) for the manifest format, trust model, installation lifecycle, rollout profiles, migration behavior, and examples.

### Plugin commands

```bash
crux plugins list
crux plugins install ./path/to/provider.plugin
crux plugins install https://example.com/provider.git
crux plugins trust PLUGIN_ID --digest SHA256
crux plugins rescan
```

Run `crux plugins --help` for the complete command and flag reference.

### Provider catalog

Provider metadata loads from a valid local cache or the catalog embedded in the binary. Crux does not contact Charm's Catwalk service during startup.

Catalog changes are explicit:

```bash
crux update-providers
crux update-providers ./providers.json
crux update-providers https://example.com/providers.json
```

With no argument, `update-providers` resets the local catalog to the embedded data. A network request is made only when an explicit HTTP or HTTPS URL is supplied.

## Using Crux

Build and install from the repository with the Go version declared in [`go.mod`](go.mod):

```bash
git clone https://github.com/example-git/crux.git
cd crux
./build.sh --install
```

Start an interactive session:

```bash
crux
```

Run a non-interactive request or resume a session:

```bash
crux run "Explain the current changes"
crux -s SESSION_ID
crux --continue
```

Common commands:

```bash
crux --help
crux models
crux login
crux accounts
crux session list
crux logs
```

Crux also includes durable plans, managed background tasks, project records, scoped memory, custom agents, and local traffic diagnostics. These features are exposed through the interactive UI and typed agent tools.

## Configuration

Crux supports JSON configuration and executable `cruxrc` shell configuration. Repository configuration is trusted input: `cruxrc` is shell code, and command substitutions in JSON can execute commands.

A minimal JSON file can reference the configuration schema:

```json
{
  "$schema": "https://raw.githubusercontent.com/example-git/crux/main/schema.json"
}
```

Within a project directory, precedence from lowest to highest is:

1. `crux.json`
2. `.crux.json`
3. `cruxrc`
4. `.cruxrc`

Crux uses `CRUX_*` environment variables and isolated Crux configuration, data, cache, database, log, and socket paths. It does not automatically read or modify legacy Crush state. An optional manual migration helper is available at [`scripts/migrate-crush-to-crux.sh`](scripts/migrate-crush-to-crux.sh).

### Provider and account backups

`crux export [archive]` creates a password-encrypted archive containing global provider configuration, installed provider bundles, plugin trust and provenance, stored accounts, and saved authenticated connections. Export prompts for the password twice and creates a private file without overwriting an existing archive.

`crux import <archive>` prompts for the archive password, authenticates and validates the complete archive before writing any data, then restores those provider and account files. Restart Crux after import so the restored configuration is loaded.

## Authenticated server connections

Remote Crux servers use TLS 1.3 mutual authentication with independently generated Ed25519 client and server identities. Only public pairing codes are exchanged; private keys stay on the machine that created them.

1. On the server, run `crux connections server-init` and give its public pairing code to the client user.
2. On the client, run `crux connections add NAME tcp://HOST:PORT SERVER_CODE`. Give the printed public client pairing code to the server user.
3. On the server, run `crux connections authorize NAME CLIENT_CODE`.
4. Start the server with `crux server --host tcp://0.0.0.0:PORT --workspace-root /srv/projects` and connect with `crux --connection NAME`.

A parameterless saved connection opens the remote workspace menu. It lists active workspaces and provides a bounded server-side directory browser for opening a new workspace. Gracefully exiting a workspace returns to this menu. Explicit `--cwd`, `--data-dir`, `--session`, `--continue`, `--yolo`, or `--channels` values bypass the menu and open the requested workspace directly.

The browser starts at the server user's home directory. Repeatable `--workspace-root PATH` options add explicitly permitted roots. Requested paths are resolved before access, must remain within one of those roots, and directory-entry symlinks are not followed. Manually paired clients are trusted workspace-management principals, but their filesystem access remains confined to these roots.

Saved connections and private identities are stored in the private Crux global data directory. Plain unauthenticated TCP is restricted to loopback use. When a saved connection creates a remote workspace, the client forwards its resolved provider configuration and active account credentials through the authenticated TLS connection. The client environment is not forwarded. The server keeps forwarded state only in workspace memory, excludes it from traffic-log bodies and API responses, and does not write it to server configuration or account files.

On Linux, `crux server daemon install --host tcp://0.0.0.0:PORT --workspace-root /srv/projects` installs and starts a user service after server identity and client authorization are configured. The command supports systemd, OpenRC user services, and runit, with `--manager` available when automatic detection is insufficient. Workspace roots are repeatable and are preserved in the generated service command. Daemon installation is rejected on macOS and Windows.

## Network diagnostics

Crux does not perform automatic Charm release or provider-catalog checks. Model providers, configured remote MCP servers, explicit web tools, remote provider discovery, HTTPS plugin installation, and explicitly enabled semantic indexing can use the network.

HTTP and WebSocket diagnostics are retained locally in `~/.ai-cli/traffic/crux.db`. Crux keeps a process-wide in-memory set of resolved API keys, OAuth tokens, client secrets, and manifest fields marked secret. Exact occurrences of those values are removed from logs, traffic diagnostics, errors, and API responses. This supplements structured field-name redaction rather than replacing it; unknown secrets and ordinary sensitive content can still appear. Treat the database as private.

## Project documents

- [`docs/provider-plugins/README.md`](docs/provider-plugins/README.md)
- [`SECURITY.md`](SECURITY.md)
- [`RELEASES.md`](RELEASES.md)
- [`BRANDING.md`](BRANDING.md)
- [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)

## License and provenance

Crux retains the repository's [`FSL-1.1-MIT`](LICENSE.md) terms and applicable upstream notices. The current FSL terms prohibit competing commercial use until the future MIT license becomes effective for the applicable source version. Notices for source integrated into the repository and embedded third-party assets are documented in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

Charmbracelet, Charm, and Crush names and marks are retained only for factual attribution, dependency identification, migration guidance, or preserved legal notices. Crux branding and runtime identity are independently maintained.
