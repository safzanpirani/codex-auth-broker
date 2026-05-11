# Security Policy

## Supported Versions

Only the latest commit on `main` is supported until the project has tagged
releases.

## Reporting A Vulnerability

Open a private security advisory on GitHub if possible. Do not include live
tokens, refresh tokens, auth files, or account identifiers in public issues.

## Scope

Security-sensitive areas:

- accidental refresh-token exposure;
- logging token values;
- public network binding without client auth;
- auth-file permission handling;
- prompt or response data leaking through logs.

The broker should never return the Codex OAuth refresh token to clients.

