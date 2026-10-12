---
name: deploy/offline-ledger
description: Break-glass for an env whose release ledger is a control plane that is unreachable — `forge env deploy <env> --offline-ledger` redeploys the cached current release by its recorded digests, builds nothing, queues the promotion, and replays it on the next successful ledger contact. Use when `forge env deploy` cannot reach the control plane and a redeploy of the live release is needed.
---

# Break-glass: the control plane is down

An env whose
ledger is a control plane cannot resolve its current release when that control
plane is unreachable, and `FORGE_LEDGER=machine` is no substitute (it holds none
of the live history and trips the unimported-ledger refusal). Every successful
ledger read or write refreshes a local copy of the env's current promotion and
release (under `$FORGE_LEDGER_HOME`, per project and env), so:

```
forge env deploy prod --offline-ledger     # redeploy the cached CURRENT release by its digests; builds nothing
```

It prints the copy's age and what it will replay, applies from this machine,
and queues the promotion record (with the apply's verdict). The next successful
ledger contact replays it, idempotently — and as a compare-and-set against the
promotion the copy named, so a colleague's promotion made meanwhile is never
overwritten (the record is refused, reported, and stays queued). It never
builds, cuts or moves to another release, refuses an env whose hosted tiers or
hub-converged clusters the control plane applies, and is the only thing that
bypasses the unimported-ledger refusal. A machine with no copy is told so:
rehearse it once while the control plane is up (`forge env status <env>`).
