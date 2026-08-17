# Security policy

## Reporting a vulnerability

If you believe you've found a security vulnerability in 1-bit-bridge, **please do not open a public GitHub issue**. Public issues will tip off a would-be attacker before a fix can ship.

Instead, report it privately via GitHub's [Security Advisories](https://github.com/acoseac/1-bit-bridge/security/advisories/new) (the *Report a vulnerability* button in the repo's **Security** tab). That flow is encrypted and only visible to the maintainers.

If GitHub's flow doesn't work for you, email `support@1-bit.app` with the word `[1-bit-bridge-security]` in the subject.

## What to include

- Affected version(s) — run `bridge version` on a reachable install, or note the release tag / commit hash.
- A clear description of the issue and, where possible, a reproducer (config snippet, request sequence, or script). Minimal self-contained reproducers shorten time-to-fix by days.
- Your assessment of impact: who can exercise the bug (LAN peer, paired client, operator-only), and what they could achieve (read arbitrary files, escape the library root, impersonate a paired device, …).
- Whether you'd like to be credited in the fix's release notes, and how you want to be attributed.

## What happens next

- Acknowledgement within **3 business days**. If you don't hear anything in that window, the email may have gotten lost — ping the GitHub advisory flow as a backup.
- A fix target agreed with you and tracked against a private branch until released. Typical window for a high-severity issue is 14 days, longer for changes that need protocol renegotiation with the [1-bit iOS app](https://github.com/acoseac/1-bit).
- Public disclosure in the release notes once the fix ships. If you reported the issue, you get a credit line (with whatever attribution you chose) unless you've told us otherwise.

## In scope

- The bridge HTTP API (`/v1/*`), the admin console (`/`, `/api/*`, `/static/*`), the manifest store, the auth / token handling, the scanner, the enrichers, and any code path reachable from a request or a config file.
- The `bridge://pair?…` URL scheme parser in the iOS app if the issue is reachable via a malformed bridge-issued URL.
- Install / packaging paths on any supported OS (Developer ID / notarization bypass, launchd / systemd / Windows Service privilege escalation).

## Out of scope

- Reports that require physical access to the machine running the bridge or the token store on disk.
- Denial of service via excessive request volume on a LAN — rate limiting isn't a project goal; run it behind a VPN for trusted-peer access.
- Vulnerabilities in third-party dependencies that are already disclosed upstream, unless 1-bit-bridge's usage amplifies the impact (in which case, please tell us — happy to pin or patch).
- Reports produced entirely by automated scanners without a demonstrated impact.
