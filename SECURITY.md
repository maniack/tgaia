# Security policy

## Supported versions

Security fixes are applied to the latest version on the default branch. Until the
project publishes its first stable release, users should deploy an explicitly pinned
commit or container digest and review changes before upgrading.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting feature for this repository. Do
not open a public issue containing credentials, private message content, exploit code,
or details that would put a deployed instance at risk.

Include the affected version or commit, deployment mode, reproduction steps, expected
impact, and any suggested mitigation. Remove Telegram tokens, OIDC credentials, LLM
keys, database DSNs, chat content, user identifiers, and infrastructure names from the
report unless they are essential; use clearly marked placeholders instead.

## Deployment responsibility

tgaia processes private conversations and can invoke external tools. Operators are
responsible for configuring TLS, OIDC, database access, Telegram webhook secrets, MCP
allowlists, network policy, backups, retention, and provider-side data controls. Never
commit populated configuration or runtime databases.
