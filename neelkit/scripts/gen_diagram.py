"""Generates docs/system-diagram.svg for the NeelKIT README.

Usage: python3 scripts/gen_diagram.py > docs/system-diagram.svg

Coordinates are laid out on a fixed grid so every arrow meets a box edge:
  left column   x  48..288   things outside the server (clients, models, Ollama)
  server        x 328..968   neelkitd, two inner columns of 290 px
  right column  x 1008..1232 things on disk the server owns
"""
import sys
from xml.sax.saxutils import escape

W, H = 1280, 860
SANS = "Inter, 'Segoe UI', system-ui, -apple-system, Helvetica, Arial, sans-serif"
MONO = "'JetBrains Mono', 'SF Mono', Consolas, 'Liberation Mono', monospace"

C = dict(
    bg="#0b0d12", card="#141821", card2="#10131a", stroke="#262c38",
    text="#f2f4f7", text2="#a3acbb", text3="#6b7383",
    blue="#3b6cff", violet="#7c5cff", cyan="#22d3ee",
    green="#34d399", amber="#f5a524", rose="#fb7185",
)

out = []
add = out.append


def text(x, y, s, size=13, color=None, weight=400, family=SANS, anchor="start", spacing=None):
    ls = f' letter-spacing="{spacing}"' if spacing else ""
    add(f'<text x="{x}" y="{y}" font-family="{family}" font-size="{size}" '
        f'font-weight="{weight}" fill="{color or C["text"]}" text-anchor="{anchor}"{ls}>'
        f'{escape(s)}</text>')


def box(x, y, w, h, title, lines=(), accent=None, fill=None, mono_lines=False, tag=None):
    add(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" '
        f'fill="{fill or C["card"]}" stroke="{C["stroke"]}"/>')
    if accent:
        add(f'<rect x="{x}" y="{y + 12}" width="3" height="{h - 24}" rx="1.5" fill="{accent}"/>')
    text(x + 16, y + 26, title, 14, weight=600)
    if tag:
        text(x + w - 14, y + 26, tag, 10.5, C["text3"], 600, MONO, "end", "0.6")
    for i, ln in enumerate(lines):
        text(x + 16, y + 48 + i * 19, ln, 12 if mono_lines else 12.5,
             C["text2"], family=MONO if mono_lines else SANS)


def arrow(points, color=None, dashed=False, label=None, label_at=None, label_anchor="middle", head=True):
    color = color or C["text3"]
    d = "M " + " L ".join(f"{x} {y}" for x, y in points)
    dash = ' stroke-dasharray="5 5"' if dashed else ""
    marker = f' marker-end="url(#h-{color[1:]})"' if head else ""
    add(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="1.6"{dash}{marker}/>')
    if label:
        lx, ly = label_at
        text(lx, ly, label, 10, C["text3"], 500, MONO, label_anchor)


def pill(x, y, w, label, color):
    add(f'<rect x="{x}" y="{y}" width="{w}" height="24" rx="12" fill="{color}" fill-opacity="0.12" '
        f'stroke="{color}" stroke-opacity="0.45"/>')
    text(x + w / 2, y + 16, label, 11.5, color, 600, anchor="middle")


add(f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" '
    f'role="img" aria-labelledby="t d">')
add('<title id="t">NeelKIT system architecture</title>')
add('<desc id="d">Clients talk to the neelkitd Go server over REST and WebSocket. Audio flows through '
    'a realtime session with Silero VAD, Parakeet speech recognition, transcript repair and an intent '
    'router (grammar first, optional local LLM). The assistant passes proposed tool calls through a '
    'policy gate and an os.Root sandbox confined to one workspace folder. State is kept in SQLite.</desc>')

# Defs: arrowheads in each colour used, and the signature gradient.
add('<defs>')
for col in (C["text3"], C["violet"], C["amber"], C["green"], C["cyan"], C["rose"]):
    add(f'<marker id="h-{col[1:]}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" '
        f'orient="auto-start-reverse"><path d="M0 0 L10 5 L0 10 z" fill="{col}"/></marker>')
add(f'<linearGradient id="sig" x1="0" y1="0" x2="1" y2="1">'
    f'<stop offset="0" stop-color="{C["blue"]}"/><stop offset="0.5" stop-color="{C["violet"]}"/>'
    f'<stop offset="1" stop-color="{C["cyan"]}"/></linearGradient>')
add('</defs>')

# Canvas
add(f'<rect width="{W}" height="{H}" rx="20" fill="{C["bg"]}"/>')
text(48, 58, "NeelKIT · system architecture", 22, weight=700)
text(48, 84, "Everything runs on your machine. Solid arrows follow one spoken command from microphone to result.",
     13, C["text2"])

# Column headings
for x, label in ((48, "CLIENTS & LOCAL SERVICES"), (1008, "ON DISK")):
    text(x, 118, label, 10.5, C["text3"], 600, MONO, spacing="1.2")

# ---------------------------------------------------------------- left column
box(48, 130, 240, 166, "Web workstation", (
    "Next.js 16 · React 19",
    "Mic via AudioWorklet, 16 kHz",
    "Live transcript + spectrum",
    "Approval cards",
    "History · tools · setup",
), accent=C["cyan"])
box(48, 316, 240, 64, "nk CLI", ("ask · plan · transcribe · audit",), accent=C["cyan"])
box(48, 398, 240, 84, "Speech models", ("Parakeet TDT 0.6B (int8)", "Silero VAD"), accent=C["violet"],
    tag="models/")
box(48, 586, 240, 110, "Ollama", ("Local LLM, e.g. qwen2.5", "Optional: built-in commands", "work without it"),
    accent=C["violet"], tag=":11434")

# ---------------------------------------------------------------- server shell
add(f'<rect x="328" y="118" width="640" height="694" rx="18" fill="{C["card2"]}" '
    f'stroke="url(#sig)" stroke-width="1.5"/>')
add(f'<rect x="348" y="104" width="252" height="26" rx="13" fill="{C["bg"]}" stroke="url(#sig)" stroke-width="1.2"/>')
text(474, 122, "neelkitd · Go 1.24 · one binary", 12, C["text"], 600, anchor="middle")

# API bar
add(f'<rect x="348" y="150" width="600" height="56" rx="12" fill="{C["card"]}" stroke="{C["stroke"]}"/>')
text(364, 174, "HTTP API", 14, weight=600)
text(932, 174, "API-key auth · rate limits · request IDs", 11.5, C["text3"], anchor="end")
text(364, 195, "REST /v1/*  ·  WebSocket /v1/realtime (single-use ticket)  ·  OpenAPI-checked",
     12, C["text2"], family=MONO)

# Left inner column: voice -> understanding
box(348, 228, 290, 150, "Realtime session", (
    "Ring buffer · incremental mel",
    "Silero VAD endpointing + pre-roll",
    "Previews: async, one at a time",
    "Final: one decode per utterance",
), accent=C["cyan"])
box(348, 398, 290, 84, "Speech recognition", ("Parakeet TDT · ONNX Runtime", "Encoder + decoder worker pool"),
    accent=C["violet"])
box(348, 502, 290, 64, "Transcript repair", ("“feature dash auth” → feature-auth",), accent=C["violet"])
add(f'<rect x="348" y="586" width="290" height="110" rx="12" fill="{C["card"]}" stroke="{C["stroke"]}"/>')
add(f'<rect x="348" y="598" width="3" height="86" rx="1.5" fill="{C["violet"]}"/>')
text(364, 612, "Intent router", 14, weight=600)
pill(364, 626, 258, "1 · Grammar · 16 rules · 0 tokens", C["green"])
pill(364, 658, 258, "2 · Local LLM, if grammar misses", C["violet"])

# Right inner column: execution
box(658, 228, 290, 70, "Assistant", ("Turn state machine · audit first",), accent=C["blue"])
add(f'<rect x="658" y="318" width="290" height="120" rx="12" fill="{C["card"]}" stroke="{C["stroke"]}"/>')
add(f'<rect x="658" y="330" width="3" height="96" rx="1.5" fill="{C["amber"]}"/>')
text(674, 344, "Policy gate", 14, weight=600)
text(932, 344, "decisions only escalate", 10.5, C["text3"], 600, MONO, "end")
for i, (k, v, col) in enumerate((
    ("READ", "runs immediately", C["green"]),
    ("WRITE", "asks first", C["amber"]),
    ("EXECUTE", "always asks", C["rose"]),
    ("NETWORK", "asks · private IPs refused", C["cyan"]),
)):
    y = 366 + i * 18
    text(674, y, k, 11, col, 700, MONO, spacing="0.6")
    text(752, y, v, 12, C["text2"])
box(658, 458, 290, 110, "Sandbox", (
    "os.Root: kernel-enforced boundary",
    "argv only · no shell",
    "Clean environment · secrets hidden",
), accent=C["rose"])
box(658, 588, 290, 108, "Built-in tools", ("fs · git · notes", "search · shell tasks", "Typed schema per tool"),
    accent=C["blue"])

# Storage bar
add(f'<rect x="348" y="716" width="600" height="74" rx="12" fill="{C["card"]}" stroke="{C["stroke"]}"/>')
text(364, 742, "SQLite · WAL + FTS5", 14, weight=600)
text(364, 765, "conversations · transcripts (searchable) · tool executions · audit log · API-key hashes",
     12, C["text2"])

# ---------------------------------------------------------------- right column
box(1008, 130, 224, 308, "Legend", (), fill=C["card2"])
legend = (
    (C["text3"], False, "request path"),
    (C["violet"], False, "model inference"),
    (C["violet"], True, "optional LLM fallback"),
    (C["rose"], False, "confined to workspace"),
    (C["amber"], True, "approval round-trip"),
    (C["green"], False, "persisted to disk"),
)
for i, (col, dashed, label) in enumerate(legend):
    y = 168 + i * 27
    dash = ' stroke-dasharray="5 5"' if dashed else ""
    add(f'<path d="M 1026 {y} L 1066 {y}" stroke="{col}" stroke-width="1.6"{dash} '
        f'marker-end="url(#h-{col[1:]})"/>')
    text(1078, y + 4, label, 12, C["text2"])
text(1024, 344, "Defaults", 12, C["text"], 600)
for i, ln in enumerate(("Listens on 127.0.0.1 only", "No unauthenticated mode", "No file-delete tool",
                        "Nothing leaves the machine")):
    text(1024, 366 + i * 17, "· " + ln, 12, C["text2"])

box(1008, 458, 224, 110, "Workspace folder", ("The only path tools", "can reach", "set with -workspace"),
    accent=C["rose"])
box(1008, 716, 224, 74, "data/", ("neelkit.db", ), accent=C["green"], mono_lines=True)

# ---------------------------------------------------------------- arrows
# Clients into the API
arrow([(288, 172), (346, 172)], label="WS+REST", label_at=(317, 164))
arrow([(288, 348), (316, 348), (316, 196), (346, 196)])
# Models loaded by the recogniser
arrow([(288, 440), (346, 440)], C["violet"], label="load", label_at=(317, 432))
# Voice pipeline, top to bottom
arrow([(493, 206), (493, 226)])
arrow([(493, 378), (493, 396)])
arrow([(493, 482), (493, 500)])
arrow([(493, 566), (493, 584)])
# Router -> LLM (optional), drawn leftward
arrow([(348, 670), (290, 670)], C["violet"], dashed=True)
# Router -> assistant through the gutter
arrow([(638, 641), (648, 641), (648, 263), (656, 263)])
# Execution, top to bottom
arrow([(803, 298), (803, 316)])
arrow([(803, 438), (803, 456)])
arrow([(803, 568), (803, 586)])
# Approval round-trip: policy gate back up to the browser via the API
arrow([(948, 330), (958, 330), (958, 178), (950, 178)], C["amber"], dashed=True)
# Sandbox confined to the workspace
arrow([(948, 513), (1006, 513)], C["rose"])
# Persistence
arrow([(948, 753), (1006, 753)], C["green"])
arrow([(803, 696), (803, 714)], C["green"])

add('</svg>')
sys.stdout.write("\n".join(out))
