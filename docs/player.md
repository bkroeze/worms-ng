# Player guide

Worms NG is a six-direction, territory-capture game. A worm moves between
neighboring dots, leaving an edge (trail) behind. An edge cannot be reused. A
territory is captured when its sixth boundary trail is laid; the worm that lays
that trail receives the point. The game ends when every worm is dead or the game
is otherwise ended.

The browser client is served from the root URL after the server starts. It uses
the same versioned API as agents and operators. A deployment may provide its own
rules payload and participants, but the board geometry and six canonical
 directions remain part of the current rules contract.

Before playing, confirm the deployment passed the [supported browser and
viewport matrix](browser-support.md). The browser path must remain on the
versioned API origin; a canvas that appears without an `ok` SQLite health
response is not a playable state.

## A move

A player or agent must choose one of six directions from the current observation.
The canonical direction order is East, SouthEast, SouthWest, West, NorthWest,
NorthEast. A move can fail when the edge already exists, the destination is
occupied, or the worm is no longer alive. A worm with no legal move is removed
by the game rules.

Human clients should submit the current game cursor and event hash with each
write. If another writer advanced the game first, the server returns `409`; load
the game again rather than overwriting the other move.

## Teaching decisions

A brain can request a teaching decision when a local pattern is unknown. The
client must answer with the pending worm ID, mask, and request number from the
current state. A stale or mismatched answer is rejected with `409`. Do not cache
a teaching request across a reload or resume operation.

## Resume and pause

`GET /api/v1/games/{id}/resume` returns the game plus the latest reconstructable
state. `POST /api/v1/games/{id}/pause` changes the status and still requires the
current cursor/hash. Resuming a game does not make stale clients current; always
use the returned cursor and event hash.

For command-line replay and verification, see [brain debugging and replay](brain-debug.md).
