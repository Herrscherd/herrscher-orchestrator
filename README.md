# herrscher-orchestrator

**The conversation-policy edge.** Decides how a turn is run: primes the prompt
with a background block recalled from Memory, records the turn as a bounded
rolling transcript on the session node, and curates that memory over time. It is
a pure plugin — no `main`; it self-registers from `init()` and the host enables
it with a blank import plus a rebuild.

## Role · Category · Ports · Config · Status · Repo

| Aspect | Value |
|--------|-------|
| **Role** | Runs the per-turn memory policy for one session (prime, observe, react, consolidate). |
| **Category** | Orchestrator (`contracts.CategoryOrchestrator`, `Kind: "basic"`) |
| **Ports implemented** | `contracts.Orchestrator` (which embeds `contracts.CurationHook`), `contracts.TurnReactor` |
| **Config & env** | Host-supplied keys (no env): `session`, `memory.project`, `memory.agent`, `memory.extractor`, `memory.journal`, `memory.consolidate-every`. Declared settings: `stale-days` / `AGENT_STALE_DAYS` (default: `30`), `archive-days` / `AGENT_ARCHIVE_DAYS` (default: `90`), `merge-min-nodes` / `MEMORY_MERGE_MIN` (default: `0`, off), `merge-target` / `MEMORY_MERGE_TARGET` (default: `stale`), `merge-max` / `MEMORY_MERGE_MAX` (default: `40`), `report-enabled` / `MEMORY_REPORT_ENABLED` (default: `true`), `report-prefix` / `MEMORY_REPORT_PREFIX` (default: `reports/`), `promote-min-age-days` / `MEMORY_PROMOTE_MIN_AGE_DAYS` (default: `0`, off), `idle-days` / `MEMORY_IDLE_DAYS` (default: `0`, off), `idle-hours` / `MEMORY_IDLE_HOURS` (default: `2`), `raw-archive` / `MEMORY_RAW_ARCHIVE` (default: off). None are required. |
| **Status** | live |
| **Repo** | [herrscher-orchestrator](https://github.com/Herrscherd/herrscher-orchestrator) |

## Install

```bash
herrscher plugin add github.com/Herrscherd/herrscher-orchestrator
```

## Two shapes, one plugin

The factory returns a `Curator` by default: transcript continuity only,
`Consolidate` is a no-op. When `memory.extractor` names an `Extractor` that was
registered via `orchestrator.RegisterExtractor(name, ex)` — from an `init()`
plugged in by blank import — it returns a `Learner` instead, which runs the
extractor over the call journal plus the transcript and persists facts (shared
project scope) and skills (private agent scope). This package ships no
extractor, so an unconfigured host gets the `Curator`.

Only `Learner` honours the merge, report, promote, idle, and raw-archive
settings; the idle trigger also needs the host to call `Learner.Start(ctx)`.

## Marker round trip

`React` handles two in-band markers the model may emit and strips them from the
reply: `<remember>fact</remember>` stores it durably, `<recall>query</recall>`
searches memory and surfaces the hits in the *next* `Context`. Every `Context`
returns a `<memory>` block carrying a short preamble that advertises this, so it
is never `""` — except with a nil Memory, which is valid and degrades to
answering without continuity.

Recalled titles and bodies are attacker-controlled (shared project memory is
multi-writer), so the `→` transcript separator is defanged to `->` everywhere;
content is capped at 100 runes, replies at 200, the transcript at 20 turns.

## Further reading

- [Herrscher docs](https://github.com/Herrscherd/herrscher-docs) — `plugins/orchestrator`
- [contracts](https://github.com/Herrscherd/herrscher-contracts) — port signatures
