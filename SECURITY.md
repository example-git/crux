# Security Policy

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability.

Use [GitHub private vulnerability reporting](https://github.com/example-git/crux/security/advisories/new) and include:

- affected version or commit
- platform and configuration
- reproduction steps or a proof of concept
- expected impact
- any known mitigation

Do not include live credentials, private model transcripts, or unrelated user data. Use synthetic evidence whenever possible.

Maintainers will acknowledge a complete report through the private advisory, investigate it, and coordinate remediation and disclosure there. Response timing depends on maintainer availability; no guaranteed service-level agreement is offered.

## Supported versions

Until the first stable Crux release, only the current `main` branch is supported. After stable releases begin, this file will list supported release lines explicitly.

## Scope

Security-sensitive areas include credential storage and redaction, provider authentication, HTTP and WebSocket transports, MCP execution, shell and file tools, plugin trust, persisted state, local IPC, traffic diagnostics, and release automation.
