# v0.2.2 — release notes draft (operator pastes into the drafted GitHub Release)

> Working copy for the operator: a line per operator-visible change, as the PRs
> land (ops/release-playbook.md, section 0 step 3), turned into the release's
> notes on release day (section 4 step 5).

---

**A one-line installer for Linux** (backlog B475): `curl -fsSL https://1-bit.app/install.sh | sh` installs the bridge on moOde, Raspberry Pi OS, Volumio, Debian or Ubuntu (64-bit, amd64 or arm64) as the user's own service, from the release it ships with and checked against the release's `checksums.txt`, sets it up with `bridge init`, and says how to pair the phone; run again, it upgrades through `bridge update`. The script is a release asset (`install.sh`), stamped with its release.
