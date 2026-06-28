# Security

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub. Open a report under the repository's
[Security Advisories](https://github.com/larsakerlund/loft/security/advisories/new) page rather than a
public issue or pull request. Include the version or commit, a description, and steps to reproduce.

Please do not file a public issue for a security problem, and give us time to ship a fix before
disclosing it.

## Scope

Loft is a backend that a reverse proxy fronts, so the parts that matter most are the trust boundary
and tenant isolation:

- `loftd` trusts only what the proxy forwards: a validated bearer token, the site derived from the
  hostname, and `Sec-Fetch-Site`. See the [trust model](ARCHITECTURE.md#trust-model).
- Each site's data is isolated by Postgres row-level security under a non-superuser role.

A report that shows a way to read or write another site's data, to reach the API without a valid
token, or to drive a deploy from a hosted site is especially valuable.

## Supported versions

The latest published version on the `0.x` line receives fixes. There is no long-term support branch
before a `1.0` release.
