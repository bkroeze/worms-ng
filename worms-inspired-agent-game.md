---
date: "2026-08-01"
tags: [game-design, atari, worms, cellular-automata, agent-game, grid-game, strategy]
status: "Research / replication concept"
description: "Research notes and a replication-oriented specification for Electronic Arts' 1983 Atari 8-bit game Worms?, with proposed extensions for AI agents."
source: "https://github.com/savetz/worms"
---

# Worms? — Atari 8-bit Rules and Replication Notes

## Identification

The game is **Worms?**, not the later platform/action game *Worms*. It was designed by **David S. Maynard** and published by **Electronic Arts** in 1983 for Atari 8-bit computers, with a Commodore 64 version. It is an interactive version of **Paterson's Worms** and a territory-capture puzzle based on the logic of *Dots and Boxes*.

The original source code was released under the MIT License and is available in the [`savetz/worms`](https://github.com/savetz/worms) repository. The Atari implementation is in Forth, with source files `Source/Atari/A.forth` and `Source/Atari/B.forth`. The repository also contains Atari disk images and links to scanned source/development notes.

## Core Game

The screen is a field of dots. Each dot is the center of a six-sided hexagonal territory. Worms—called **SAILs**, or Super Artificially Intelligent Lifeforms—move from dot to adjacent dot and leave colored trails behind them.

### Objective

Capture territories by being the worm that lays the **sixth trail** bordering a territory.

- Every trail connects two neighboring dots.
- A trail lies half in each of the two territories it borders.
- Each territory has six possible surrounding trails.
- When the sixth trail is laid, that worm's color owns the territory permanently and scores one point.
- The completed territory flashes when captured.
- A territory's color does not change again after completion.

The winner is the worm with the highest score after all worms have died or the game ends.

## Board Geometry

Use a hexagonal lattice rather than a square grid.

Each dot has six neighbor directions. A convenient axial-coordinate representation is:

```text
q,r neighbors:
(1,0), (0,1), (-1,1), (-1,0), (0,-1), (1,-1)
```

The visual implementation may use pointy-top or flat-top hexagons, but the logical board must preserve six neighbors per dot and six edges per territory.

A useful model is:

```text
Dot = lattice vertex
Trail = undirected edge between adjacent dots
Territory = hexagonal face bounded by six trails
```

The original game uses a single-screen board. For a modern recreation, make board dimensions configurable while retaining a compact default board.

## Worm Movement

Each worm occupies a dot and chooses one of six directions. It cannot:

- Move along an existing trail.
- Move into a dot occupied by another live worm.
- Continue when no legal adjacent destination remains.

When a worm moves from dot A to adjacent dot B:

1. Add the edge A–B to the trail map.
2. Color the new trail with that worm's color.
3. Update the local territory borders.
4. Recolor any incomplete territory touched by the new trail to the moving worm's color, as described by the manual.
5. If a territory now has six trails, permanently assign it to the moving worm and award one point.
6. Move the worm to B.

A worm that has no legal move is removed from the board. The game ends when all worms are dead.

A worm may not reuse an edge. This means a worm's trail is a growing path through previously unused edges. Other worms may interact with existing trails by changing the color of incomplete territories, but no worm can traverse an already-laid trail.

## The Key AI Mechanic: Local Pattern Memory

The human player does not directly script a complete route. Instead, the player supplies a direction whenever a worm encounters a local trail configuration it does not recognize.

The worm observes the pattern of trail segments around its current dot. The exact original representation can be understood as the occupancy of the six possible local directions, together with the incoming direction / orientation needed to interpret the pattern. Conceptually:

```text
state = local trail configuration + approach orientation
action = one of six movement directions
```

The worm maintains a lookup table from observed local movement patterns to chosen directions.

### Training behavior

1. A **NEW** worm begins with no useful learned rules.
2. The player chooses its direction when it reaches an unrecognized local configuration.
3. The worm stores the chosen action for that configuration.
4. When it later encounters the same configuration, it automatically repeats the stored action.
5. It continues autonomously until it reaches an unrecognized configuration or has no legal move.
6. More movement means more remembered situations and less direct player input.

The manual's example trains a zigzag:

- Move right from the initial state.
- At the next configuration, direct it lower-left.
- At the next configuration, direct it right.
- When the earlier configuration recurs, the worm automatically chooses lower-left.
- The learned sequence continues until a new local configuration appears.

This is not ordinary pathfinding. It is a reactive finite-state controller induced by play.

## Worm Types

At the start, choose a type for each of up to four worm colors / players:

| Type | Behavior |
|---|---|
| **NEW** | Starts untrained. The player teaches it during play. |
| **AUTO** | Like NEW, but the computer trains it to make smart moves during play. |
| **WILD** | The computer trains it randomly for all possible moves before the game begins. |
| **SAME** | Reuses the worm that played this color in the previous game. |
| **-----** | Asleep; this color does not play. |

The original game supports one to four worms. A modern recreation should support human control, agent control, and scripted/random control as separate policies.

## Input and Controls

From the Atari manual:

- **Select** chooses the worm position/type at setup.
- **Option** cycles the worm type for that position.
- **Start** begins the game.
- During play, the active worm flashes and its name is underlined.
- **Keyboard:** press the space bar / choose a direction using the keyboard interaction.
- **Paddles:** turn the paddle to choose direction; press the red button to move.
- Keyboard and paddles may be mixed in one game.
- Paddle port 1 controls the gold and pink worms; port 2 controls blue and green worms.

Useful commands:

| Key | Function |
|---|---|
| `1`–`9` | Set worm speed; 1 slowest, 7 default, 9 fastest |
| `+` / `-` | Increase or decrease worm speed during play |
| `ESC` | Freeze/unfreeze the game |
| `G` | Toggle grid dots off/on; available at beginning of game |
| `F` | Toggle persistent flashing for captured territories |
| `S` | Save worms to disk |
| `L` | Load worms from disk |
| `U` | Update an existing saved worm with new moves |
| `D` | Directory of saved worms |

## Turn and Timing Model for a Recreation

The historical game is visually continuous, but a clean modern implementation can use discrete simulation ticks:

```text
repeat until game over:
    for each live worm in deterministic order:
        if its controller knows the current local pattern:
            propose remembered direction
        else:
            request / generate a direction
        validate the direction
        if valid:
            apply movement and territory updates
        else:
            mark worm dead if no legal action remains
    render trails, worms, scores, and flashes
```

To stay close to the original, process one worm action at a time and make the active worm visually distinct. To support agents, expose a synchronous action API with one observation and one action per decision point.

## State Model

A replication-oriented state can contain:

```text
Board:
  lattice dimensions
  dot coordinates
  territory faces
  edge-to-territory adjacency
  trail occupancy and owner/color

Worm:
  id / color
  current dot
  alive/dead
  score
  controller type
  learned pattern -> direction table
  previous direction / orientation

Game:
  active worm
  tick / move count
  speed
  captured territories
  event log
```

Use an immutable event log during development. Events should include:

- `worm_moved(worm, from, to, edge)`
- `trail_claimed(edge, color)`
- `territory_color_changed(territory, color)`
- `territory_captured(territory, worm, color)`
- `worm_blocked(worm)`
- `worm_died(worm)`

This makes it possible to replay games and train agents on exact observations.

## Observation for an Agent

At each decision point, give an agent:

1. Its worm's current dot and orientation.
2. The six neighboring dots and whether each is occupied.
3. The six outgoing trail states: empty, own color, or another color.
4. Nearby territory completion counts.
5. Current scores.
6. Its learned local pattern table, or only the matching entry depending on the experiment.
7. Whether the game is in training mode or autonomous mode.

A compact observation encoding could be a six-bit trail mask plus a six-bit occupancy mask and orientation:

```text
observation = (trail_mask, occupied_mask, incoming_direction, local_territory_counts)
```

For strict historical behavior, learned rules should key primarily on local trail geometry and orientation, not on a global map or future planning.

## Scoring and End Conditions

- One point per captured territory.
- The worm that lays the sixth surrounding trail receives the point.
- Completed territories flash.
- A worm that cannot move dies/is removed.
- The game ends when all worms are dead.
- The winner is the highest-scoring worm.
- Ties should be displayed rather than broken arbitrarily.

## What Must Be Replicated for Fidelity

The following are the essential identity of the original game:

- Six-direction hexagonal lattice.
- Trails are permanent, non-traversable edges.
- Territories require six surrounding trails.
- Incomplete territory color follows the most recent trail color.
- Sixth trail awards the territory to its worm.
- Worms learn a local configuration-to-direction mapping through interaction.
- Learned behavior takes over automatically on recognized configurations.
- Multiple worm types: NEW, AUTO, WILD, SAME, asleep.
- A single-screen, visually legible board with active-worm cues.

## Proposed New Ideas for an Agent-Based Version

These are extensions, not claims about the Atari original.

### 1. Agent tournament mode

Run several agents with identical or different memory constraints:

- Reactive lookup-only agent
- Greedy territory agent
- Monte Carlo agent
- Reinforcement learner
- LLM agent receiving textual or structured observations

Compare score, survival time, territory efficiency, and generalization to unseen local patterns.

### 2. Separate training and execution phases

Allow an agent to train a worm through demonstrations, then freeze its lookup table and test it on new boards. This directly tests how much of the original's charm comes from learned local automata.

### 3. Pattern-library sharing

Let worms inherit or exchange local rules. Add controlled experiments for:

- No sharing
- Same-color team sharing
- All-worm shared library
- Corrupted or noisy rule sharing

### 4. Modern strategic layer

Keep the historical worm controller but add an optional strategic planner that chooses which unrecognized local state to teach. The planner can optimize territory capture or survival while the worm remains locally reactive.

### 5. Explainable agent behavior

Display the exact reason for each autonomous move:

```text
recognized pattern 0b101011 + orientation 3 → move direction 1
```

This makes the game useful for studying small agents rather than turning it into an opaque neural-network demo.

### 6. New terrain and rule variants

Optional variants could add:

- Obstacles or holes in the hex lattice
- Weighted territories
- One-way trails
- Temporary trails
- Worm energy limits
- Team colors
- Fog of war
- Board generation from a seed
- Hexagonal worlds larger than one screen

Keep these behind a ruleset switch so the classic mode remains a stable reference.

## Implementation Plan for a Coding Agent

### Milestone 1 — deterministic engine

- Implement axial hex lattice.
- Enumerate dots, edges, and territory faces.
- Add trail placement and six-trail capture.
- Add collision and death rules.
- Write replayable unit tests.

### Milestone 2 — classic renderer

- Render dots, trails, territories, worms, score, and active-worm cue.
- Add speed control and pause.
- Add game setup for up to four worms.

### Milestone 3 — learned local controller

- Encode local trail configuration and orientation.
- Implement NEW learning behavior.
- Implement AUTO and WILD policies.
- Implement SAME persistence.

### Milestone 4 — agent interface

- Define a JSON observation/action protocol.
- Support human, scripted, API, and LLM agents.
- Add event replay and deterministic seeds.

### Milestone 5 — experiments and new ideas

- Add tournament mode.
- Add frozen-policy evaluation.
- Add rule sharing and strategic teaching.
- Add optional modern terrain/rulesets.

## Source Notes

Primary sources consulted:

1. [Electronic Arts Worms? Atari 8-bit manual](https://www.gamesdatabase.org/Media/SYSTEM/Atari_8_bit//Manual/formated/Worms_-_Electronic_Arts.pdf)
2. [Worms? source code by David S. Maynard](https://github.com/savetz/worms)
3. [Worms? — Wikipedia](https://en.wikipedia.org/wiki/Worms%3F)
4. [Worms? — AtariMania](https://www.atarimania.com/game-atari-400-800-xl-xe-worms_5861.html)
5. [Worms? — MobyGames](https://www.mobygames.com/game/65383/worms/)
6. [Darworms, David Maynard's browser-based descendant](https://github.com/dmaynard/Darworms)

The manual is the authority for the summarized rules. The open-source repository is the best route for resolving implementation details that the prose manual leaves ambiguous.

---

Last updated: 2026-08-01 — researched original rules, source availability, state model, and agent-oriented replication plan.

## Licensing Note

The original game source repository states that David S. Maynard released the source under the MIT License. Treat the original game title, artwork, sound, and EA branding separately from the source-code license when creating a new implementation.
