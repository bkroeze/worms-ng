# Classic rule record

This file is the checked-in interoperability record for `internal/engine`. The
normative inputs are `local://classic-rules-final.json` and the historical
manual/source notes in `worms-inspired-agent-game.md`.

## Classic contract (v1)

* The board is an 18 x 18 torus. Logical coordinates are `x,y = 1..18` and
  use odd-row-offset neighbors. Directions are absolute clockwise
  `E,SE,SW,W,NW,NE = 0..5`; opposite directions are `3,4,5,0,1,2`.
* A dot owns one territory. A trail is one canonical undirected edge, observed
  as reciprocal endpoint bits. Its adjacent territories are the two endpoint
  dots. There are 324 territories and 972 possible edges.
* `NewClassic` places every playing worm at `(9,9)`, with no initial trail or
  heading and `CRIX = NOMOVE (6)`. Multiple worms may share a dot.
* A move is legal when the current dot's selected bit is zero. Destination
  worm occupancy is irrelevant. Trails are permanent. A move mutates in this
  order: source spoke and source recolor/capture; position change; destination
  recolor; reciprocal destination spoke and destination capture. Source capture
  happens before destination capture.
* A full six-bit territory captures for the mover and awards one point. The
  source and destination are independent, so a move awards 0, 1, or 2 points.
  Incomplete territory color is per dot and follows the latest mover; it is not
  an immutable whole-edge color. Completed ownership is permanent.
* The controller key is exactly the raw low six-bit mask. It has 64 entries,
  uses absolute directions, and excludes rotation, reflection, CRIX, previous
  direction, color, worm occupancy, and history. Forced exits are
  `0x3e..0x1f -> d0..d5`; `0x3f -> DIE`.
* Setup kinds are typed: NEW starts with `rule[0]=GETNEW`, AUTO with
  `rule[0]=DOAI`, WILD has deterministic generated legal entries, and SAME /
  NAMED preserve a loaded table. Imported unsafe directional entries are
  normalized to GETNEW. Unknown input is a persistent pending decision; later
  slots cannot overtake it. Slots run in setup/index order and game-over is
  evaluated only after the complete scan.
* Capture at arrival is scored before the mover is killed. Completing the
  departed source never kills that mover. A blocked worm is handled at its own
  slot; passive worms are not asynchronously killed by another slot's capture.
  Winners are all highest-scoring worms; ties remain ties.

## Explicit modern safety boundary

`New` and `NewBounded` are **modern bounded safety constructors**, not claims
about historical behavior. They use bounded axial coordinates, reject movement
into a live worm, and reject untrusted directions without mutation. `NewClassic`
(and `NewToroidal` when configured for classic rules) is the compatibility
constructor and intentionally permits co-location and uses toroidal odd-row
geometry. Imported snapshots are validated for IDs, neighboring edges, and
canonical reciprocal occupancy; this integrity validation is modern durable
safety, even though permanent edges make the corresponding classic state
invariant. JSON snapshots are version `1`; unknown versions fail.

The engine emits append-only value events with deterministic sequence/tick
ordering and hashes canonical sorted snapshot records. A replay must reproduce
state and hash; event presentation must not be interpreted as a whole-edge
color or as a change to the source-before-destination mutation order.

## Ambiguity and compatibility register

The following decisions are explicit compatibility rules, rather than inferred
modern behavior:

| ID | Decision | Source citation | Regression vector |
|---|---|---|---|
| A1 | A territory is dot-centred and a trail is one reciprocal canonical edge. | `local://classic-rules-final.json`, “Logical dot/edge/territory geometry”; `worms-inspired-agent-game.md:17-53` | `TestClassicGeometryGolden` |
| A2 | Classic INITPOS co-locates all playing worms at `(9,9)` and uses `NOMOVE=6`; modern starts may be distinct. | `local://classic-rules-final.json`, “Initial position and orientation”; `worms-inspired-agent-game.md:110-122` | `TestClassicGeometryGolden`; `GenerateBoard` |
| A3 | Raw masks are absolute six-bit keys; rotated patterns do not collide. | `local://classic-rules-final.json`, “Local pattern key and rotation”; `worms-inspired-agent-game.md:78-108` | `RawMaskBits`; `TestClassicRawMaskEncoding` |
| A4 | Co-location is legal in classic and destination capture kills only after scoring. | `local://classic-rules-final.json`, “Blocking, collision, capture, and death timing”; `worms-inspired-agent-game.md:57-76` | `TestClassicReciprocalMaskAndDoubleCapture` |
| A5 | Unknown NEW input stalls the current indexed slot; later slots cannot overtake it. | `local://classic-rules-final.json`, “Turn order and game-over boundary”; `worms-inspired-agent-game.md:150-169` | `TestClassicRoundOrderStallsAtPendingSlot` |

## Human-readable fixture vectors

* **Move:** `(9,9), mask=000000, d0 -> (10,9)`, reciprocal masks `0x01/0x08`.
* **Shared edge:** insert `(9,9)-d0-(10,9)` once; reverse insertion is rejected.
* **Sixth edge:** source `0x3e`, destination `0x37`, move `d0`; ordered captures
  are source then destination and score delta is `+2`.
* **Blocked worm:** mask `0x3f` maps to `DIE`; status changes only at that slot.
* **Repeated oriented pattern:** `0x08` and geometric rotations remain distinct
  entries; `RawMaskBits(0x08)` is `001000` in d5…d0 display order.

Modern bounded collision rejection, distinct seeded starts, typed action errors,
and frozen controller decisions are deliberate extensions. They MUST NOT be
used to reinterpret classic co-location, shared-centre INITPOS, or raw-mask
fidelity.
