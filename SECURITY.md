# Security Policy

## Supported versions

This project follows a rolling release model: only the latest release is
supported. Please upgrade (`shuck upgrade`) before reporting an issue.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately through GitHub's [security advisories][advisories] ("Report a
vulnerability" on the **Security** tab), or contact the maintainers listed in
[CODEOWNERS](.github/CODEOWNERS).

[advisories]: https://github.com/justanotherspy/shuck/security/advisories/new

Please include:

- a description of the issue and its impact,
- steps to reproduce or a proof of concept,
- the affected version(s).

We aim to acknowledge reports within a few days and will keep you updated on
remediation progress. Once a fix ships we're happy to credit you.

## Hardening already in place

- Dependencies and GitHub Actions are kept current by Dependabot; all actions
  are pinned to commit SHAs.
- CI runs CodeQL, Semgrep, and TruffleHog secret scanning; the GitHub Actions
  workflows themselves are audited by [zizmor][zizmor].
- Release archives ship a `checksums.txt` signed with [cosign][cosign] (keyless
  OIDC); verify it against the published `checksums.txt.sigstore.json`.

[cosign]: https://github.com/sigstore/cosign
[zizmor]: https://github.com/zizmorcore/zizmor
