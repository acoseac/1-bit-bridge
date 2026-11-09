# v0.2.4 — release notes draft (operator pastes into the drafted GitHub Release)

> Working copy for the operator: a line per operator-visible change, as the PRs
> land (ops/release-playbook.md, section 0 step 3), turned into the release's
> notes on release day (section 4 step 5). The release after v0.2.3, 0.2.4 by
> plan.

---

**The embedded Tailscale node uploads no diagnostic logs unless the config allows it** (backlog B749): in `tsnet` mode the bridge sets `TS_NO_LOGS_NO_SUPPORT=true` for its node unless `tailscale.uploadDiagnosticLogs: true` is set (in `bridge.yaml`, or Settings, Networking), so an install updated from an earlier version stops uploading Tailscale's diagnostic logs, and network flow logs, at its first start of this version: earlier versions uploaded them by default, which is not taken for consent. The node keeps its identity and its state. A tailnet that requires those logs refuses the node until the key is set, and the admin tile says what to change, and, where the key is set and the uploads are off all the same, what holds them off. A change of the key takes a restart of the bridge's process: once the bridge has run its node with the key off, the uploads stay off until that process exits, so quit the bridge, its menu included, and launch it again, or restart its service (the menu's Start now, and the start `bridge init` offers, run the bridge in the same process). Turning the key off does not stop a node already running with it on from uploading until that node stops. The node's name no longer follows the library's: a node already registered keeps the name it has, so a library rename no longer renames it, and a new node registers as `1-bit-bridge` unless `tailscale.hostname` is set.

**Before going back to an earlier version, remove `tailscale.uploadDiagnosticLogs` from `bridge.yaml`** (backlog B749): a bridge from before this release refuses the key at load, whatever its value, and does not start; it also uploads Tailscale's diagnostic logs unless `TS_NO_LOGS_NO_SUPPORT=true` is in its environment.
