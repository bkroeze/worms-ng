package protocol

// EvolutionRules documents the v1 wire-compatibility contract for external
// processes. Keep this alongside the checked-in JSON schemas and fixtures.
//
// v1 consumers MUST reject an unknown version and unknown object fields. A
// compatible v1 addition is an optional field with a documented default;
// existing producers omit it and existing consumers continue to decode it.
// Required fields, enum values, direction meanings, array ordering, and the
// interpretation of a decision ID cannot change within v1. Such changes
// require v2 and a separately published schema/fixture set.
//
// Negotiation is transport-owned: an adapter advertises supported versions
// (currently only v1) before a session starts. The session selects the highest
// mutually supported version; no in-session downgrade is allowed. The
// decision-request fixture is sufficient for a non-Go process: read JSON from
// stdin, choose one legal action, and emit decision-response JSON on stdout.
const EvolutionRules = "v1 optional fields only; required/enum/order changes require v2"
