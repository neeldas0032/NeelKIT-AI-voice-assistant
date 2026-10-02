# Design system — Developer Voice Workstation

The interface is built as a workstation, not a chat app. That is a product
decision with teeth: consumer voice assistants optimise for warmth and
forgiveness, and this tool can run programs on your machine. It optimises for
legibility and for making the consequences of an action visible before it
happens.

## Foundations

**Canvas** `#08090B`. Near-black, for continuous low-light use.

**Surfaces** step by luminance, not by colour:

| Tier | Fill | Use |
|---|---|---|
| 0 canvas | `#08090B` | ground |
| 1 glass | `rgba(255,255,255,0.04)`, `blur(16px)`, 1px `rgba(255,255,255,0.08)` | panels, execution blocks |
| 2 inset | `#0D0F13`, `inset 0 1px 2px rgba(0,0,0,0.6)` | transcripts, terminal output, metrics |

Depth is **specular, not shadowed**. A tier-1 panel carries a 1px horizontal
gradient along its top edge (`rgba(255,255,255,0.16)` → `0.02`) implemented as a
`background-image` band layered over the fill. It reads as a milled bevel
catching room light, and costs one declaration instead of a stack of shadows.

**Signature gradient** `linear-gradient(135deg, #3B6CFF, #7C5CFF, #22D3EE)`.

Reserved, with no exceptions, for the live path: the voice orb, the primary
action on a surface, focused input rings, and the NeelKIT byline on an assistant
block. If it glows, audio is moving or something is about to run. The moment the
gradient is used decoratively it stops carrying information, which is the failure
mode of most gradient-heavy dark UIs.

**Semantic telemetry** is the only other coloured channel:

| | | |
|---|---|---|
| `#34D399` live | healthy daemon, a read-only tool, a passing check | |
| `#F5A524` warn | needs approval, a low-confidence word, a degraded service | |
| `#FB7185` danger | failed execution, the `execute` risk tier | |

Risk colour is always paired with the word — colour alone is not an accessible
signal, and the one place it appears is the place that must not be misread.

**Type.** Geist for interface and prose; JetBrains Mono for anything the machine
produced or will execute — transcripts, commands, paths, figures. Both
self-hosted from npm. A local-first tool that fetches its typeface from a CDN on
every page load is making a request to Google carrying the user's IP, and fails
to build on a train. The same reasoning removed the Material Symbols icon font;
the fourteen icons are inlined SVG paths.

Every figure is `font-variant-numeric: tabular-nums`. These are measurements, and
measurements that reflow as digits change are hard to compare.

### One deliberate deviation

The reference puts micro-labels at `#5A626D`. Measured against the canvas that is
**3.2:1**, on the smallest type in the product. It is raised to `#767E8A`
(**4.8:1**). The two are indistinguishable side by side and one of them is
legible.

## Layout

Tri-panel above 1280px: anchor rail (250px) · execution canvas · telemetry rail
(300px). The rail leaves first at 1280px, because the transcript is what people
read. The anchor rail becomes a drawer at 1024px and keeps every element —
including the workspace path, which is the blast radius and should never be more
than a glance away.

The workstation fills the viewport on desktop: the transcript scrolls inside its
panel, and the dock stays pinned so the control that opens the microphone is
never scrolled away mid-sentence. Below 1024px the page scrolls normally, because
a `100dvh` layout on a phone fights the URL bar.

## Execution blocks, not chat bubbles

A bubble thread says *conversation with a chatbot*. A numbered block with a
metadata header and a gutter marker says *a record of what this machine did*.
Every turn renders as a block carrying its own timestamp, its resolver tier, and
its latency.

**The approval card** is the most important surface in the product and the only
one with a coloured halo. It is built around one string — the resolved command,
set in the largest mono on the page, recessed, labelled `WILL RUN`. A server-side
integration test asserts that string describes the same argv that executes.

## Every number on screen is measured

This is the rule that shaped the telemetry rail, and it cost panels:

| Panel | Source |
|---|---|
| Last-turn latency, segmented | `transcript.final.latency_ms`, `turn.latency_ms`, per-call `duration_ms` |
| Real-time factor | `transcript.final.latency_ms ÷ duration_ms` |
| Transcript confidence | `transcript.final.confidence` |
| Per-word confidence underlines | `transcript.final.words[].confidence` |
| Resolved-by split | `turn.intent.source` counted per session |
| Device, precision, workers | `GET /v1/models` |
| Executed calls | `tool.completed` events |
| Live spectrum, 32 bands | `AnalyserNode.getByteFrequencyData` |

Panels the reference design called for that are **not** here, because the
corresponding number is not measured: tokens per second, time-to-first-token,
VRAM (this target is CPU-only), context-window occupancy, touched-file diffs, and
an AST diff viewer. A dashboard with four cards looks better than one with three.
It is only worth showing to someone who will ask where a number came from if
every card survives the question.

The waveform is the same rule applied to motion: it is real frequency data, so it
is flat when the microphone is closed. A decorative waveform that animates while
muted answers the interface's single most important question — *is it hearing
me* — wrongly rather than not at all.
