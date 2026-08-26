# herrscher-orchestrator

**The conversation-policy edge.** It decides how a turn is run: it primes the
prompt with a background block recalled from Memory, records the turn as a
bounded rolling transcript on the session node, and curates that memory over
time.

It is a pure plugin, with no `main`. It self-registers from `init()`, and the
host enables it with a blank import plus a rebuild.

Category: orchestrator (`contracts.CategoryOrchestrator`, `Kind: "basic"`).
Ports: `contracts.Orchestrator`, which embeds `contracts.CurationHook`, and
`contracts.TurnReactor`. Status: live.

## Install

```bash
herrscher plugin add github.com/Herrscherd/herrscher-orchestrator
```

## Configuration

The host supplies these directly. They have no environment binding.

| Key | What it is |
|---|---|
| `session` | the session this orchestrator runs for |
| `memory.project` | the shared project scope |
| `memory.agent` | the private agent scope |
| `memory.extractor` | the registered extractor to run, if any |
| `memory.journal` | the call journal to extract from |
| `memory.consolidate-every` | how often consolidation runs |

The declared settings are all optional, and each has an environment binding.

| Setting | Environment | Default |
|---|---|---|
| `stale-days` | `AGENT_STALE_DAYS` | `30` |
| `archive-days` | `AGENT_ARCHIVE_DAYS` | `90` |
| `merge-min-nodes` | `MEMORY_MERGE_MIN` | `0`, off |
| `merge-target` | `MEMORY_MERGE_TARGET` | `stale` |
| `merge-max` | `MEMORY_MERGE_MAX` | `40` |
| `report-enabled` | `MEMORY_REPORT_ENABLED` | `true` |
| `report-prefix` | `MEMORY_REPORT_PREFIX` | `reports/` |
| `promote-min-age-days` | `MEMORY_PROMOTE_MIN_AGE_DAYS` | `0`, off |
| `idle-days` | `MEMORY_IDLE_DAYS` | `0`, off |
| `idle-hours` | `MEMORY_IDLE_HOURS` | `2` |
| `raw-archive` | `MEMORY_RAW_ARCHIVE` | off |

## Two shapes, one plugin

The factory returns a `Curator` by default: transcript continuity only, with
`Consolidate` a no-op.

When `memory.extractor` names an `Extractor` that was registered via
`orchestrator.RegisterExtractor(name, ex)`, from an `init()` plugged in by blank
import, the factory returns a `Learner` instead. A `Learner` runs the extractor
over the call journal plus the transcript, and persists facts in the shared
project scope and skills in the private agent scope.

This package ships no extractor, so an unconfigured host gets the `Curator`.

Only `Learner` honours the merge, report, promote, idle and raw-archive settings.
The idle trigger also needs the host to call `Learner.Start(ctx)`.

## Marker round trip

`React` handles two in-band markers the model may emit, and strips them from the
reply. `<remember>fact</remember>` stores the fact durably.
`<recall>query</recall>` searches memory and surfaces the hits in the *next*
`Context`.

Every `Context` returns a `<memory>` block carrying a short preamble that
advertises this, so it is never `""`. The one exception is a nil Memory, which is
valid and degrades to answering without continuity.

Recalled titles and bodies are attacker-controlled, since shared project memory is
multi-writer. So the `→` transcript separator is defanged to `->` everywhere,
content is capped at 100 runes, replies at 200, and the transcript at 20 turns.

## Further reading

- [Herrscher docs](https://github.com/Herrscherd/herrscher-docs), page
  `plugins/orchestrator`
- [contracts](https://github.com/Herrscherd/herrscher-contracts), for the port
  signatures
