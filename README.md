# herrscher-orchestrator

**The conversation-policy edge.** This is the default `contracts.Orchestrator`: it
decides *how* a turn is run. Before each turn it primes the prompt with a compact
background block recalled from Memory; after each turn it records what happened as a
bounded rolling transcript. It is a **pure plugin** — no `main` — and self-registers
into the global plugin registry from its `init()` (xcaddy pattern), so the host
enables it with a blank import + rebuild.

> Part of the Herrscher family: **orchestrator** ·
> [contracts](https://github.com/Herrscherd/herrscher-contracts) ·
> [obsidian-memory](https://github.com/Herrscherd/herrscher-obsidian-memory) ·
> [claude-backend](https://github.com/Herrscherd/herrscher-claude-backend) ·
> [discord-gateway](https://github.com/Herrscherd/herrscher-discord-gateway) ·
> [herrscher](https://github.com/Herrscherd/herrscher) (the umbrella binary that
> imports them all).

```
require github.com/Herrscherd/herrscher-contracts  // the only dependency
```

---

## What it does

`Curator` implements `contracts.Orchestrator`. The host builds one per session
(passing the session name and the `Memory` port it composes), then drives it around
every turn:

| Method | Role |
|--------|------|
| `Context(ctx) string` | Surfaces **durable scoped memory first** (the shared project subgraph + this agent's private skills, via `contracts.RecallScoped`) then the session node and its neighbours (depth 1), as a compact `## Title` + body block to prepend to the next prompt. Returns `""` on a first turn or any recall failure — never a turn-breaking error. |
| `Observe(ctx, prompt, reply) error` | Upserts the session node with the turn prepended to a **newest-first rolling transcript**, bounded to `maxTurns` (20) lines, so the next `Context` has continuity. |
| `Consolidate(ctx) error` | The `CurationHook` seam. The default keeps the transcript bounded inline, so this is a no-op; a richer orchestrator overrides it with summarisation/pruning. |
| `Close() error` | No-op — the composed `Memory` is owned and closed by the host. |

A **nil Memory** is valid: the orchestrator still answers, just without continuity
(`Context` returns `""`, `Observe` no-ops).

The session node is keyed `sessions/<name>` with `Kind = KindSession`.

### Memory scope (P1)

`New(mem, session)` keeps the legacy behaviour (transcript-only continuity).
`NewScoped(mem, session, scope)` threads a `contracts.MemoryScope` so `Context`
also recalls the **shared project memory** (every agent of the game) and this
agent's **private skills**. The plugin entrypoint (`register.go`) reads the
`memory.project` / `memory.agent` config keys and keys them onto the spine
(`projects/<name>`, `agents/<name>`); both are optional, so an unscoped host is
unaffected.

### The learning loop (`Learner`)

`Learner` wraps the `Curator` with a real `Consolidate`: it runs a pluggable
`Extractor` over the call journal + session transcript and persists what it
returns — facts under the shared project scope, learned skills under the private
agent scope. The `Extractor` (the *what is worth remembering* heuristics) is the
closed part of the moat; this package ships only the open seam and plumbing.

**Wiring it.** The closed curation module registers its extractor by name —
`orchestrator.RegisterExtractor("roblox", ex)` from an `init()`, plugged in by a
blank import (the same pattern plugins use into the host). The entrypoint then
builds a `Learner` instead of the plain `Curator` when the host names a
registered extractor:

| Config key | Meaning |
|------------|---------|
| `memory.extractor` | name of a registered `Extractor`; unset or unknown → plain `Curator` (no learning). |
| `memory.journal` | path to the call journal fed to the extractor (e.g. `<worktree>/.neublox/calls.log`). |
| `memory.consolidate-every` | run `Consolidate` every N observed turns (`0`/unset = manual only). |

With no extractor registered the entrypoint is unchanged — an unconfigured host
keeps transcript-only continuity.

---

## Swapping it

It registers under `CategoryOrchestrator` with `Kind: "basic"`. A richer policy
(summarisation, multi-agent routing) implements `contracts.Orchestrator` and
registers under the same category to replace it — no change to the gateway, backend,
or memory ports.

---

## Safety

Transcript lines are built from attacker-controlled input (a display name, a
message). `turnLine` collapses each field to one line and defangs the `→`
separator to `->`, so a crafted message can't forge extra "author → reply" turns
inside its own line. Inbound content is capped at 100 runes, replies at 200.

Recalled memory is attacker-controlled too — **shared project memory is
multi-writer** (any agent of the game can `RecordShared`) — so `writeNode`
applies the same `→` defang to every node title/body before it reaches the
prompt, blocking a forged turn planted in a fact from spoofing another agent's
context.

---

## Build & test

```bash
go build ./...
go vet ./...
go test ./...
```

Go 1.25. Depends only on the published `herrscher-contracts`. Pure plugin — no
binary; the [herrscher](https://github.com/Herrscherd/herrscher) umbrella is the only
thing that constructs it (blank import).
