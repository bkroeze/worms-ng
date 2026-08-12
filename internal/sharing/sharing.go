// Package sharing provides deterministic, opt-in brain rule-library sharing
// experiments. It deliberately works through the store's small brain-version
// surface and never mutates a source version.
package sharing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"worms.ng/internal/engine"
	"worms.ng/internal/store"
)

// Policy identifies the sharing experiment. The values are part of persisted
// provenance and must remain stable.
type Policy string

const (
	NoSharing          Policy = "none"
	SameTeamSharing    Policy = "same_team"
	AllWormSharing     Policy = "all_worms"
	SeededNoisySharing Policy = "seeded_noisy"
)

// Compatibility aliases make the policy names easy to discover without
// introducing a second set of persisted values.
const (
	PolicyNoSharing   = NoSharing
	PolicySameTeam    = SameTeamSharing
	PolicyAllWorm     = AllWormSharing
	PolicySeededNoisy = SeededNoisySharing
)

// Descriptive aliases for callers that prefer experiment-oriented names.
type ExperimentConfig = Config
type ExperimentOutput = Output
type WormSource = Source

const (
	SharingNone        = NoSharing
	SharingSameTeam    = SameTeamSharing
	SharingAllWorm     = AllWormSharing
	SharingAllWorms    = AllWormSharing
	SharingSeededNoisy = SeededNoisySharing
	SharingSeededNoise = SeededNoisySharing
)

var (
	ErrInvalid = errors.New("sharing: invalid configuration")
	ErrMissing = errors.New("sharing: missing source")
)

// Source identifies one immutable source brain version. Rules is optional
// when DeriveFromStore is used; supplying it makes Derive useful without a DB.
type Source struct {
	WormID         string          `json:"worm_id"`
	Team           string          `json:"team,omitempty"`
	BrainVersionID string          `json:"brain_version_id,omitempty"`
	Rules          json.RawMessage `json:"rules,omitempty"`
}

// SourceFromWorm adapts an engine worm to a canonical persisted rule source.
// The team is experiment metadata; engine.Worm intentionally has no team
// field. The returned payload is never backed by the worm's mutable array.
func SourceFromWorm(w engine.Worm, team string) Source {
	values := make([]int, len(w.Rules))
	for i, action := range w.Rules {
		values[i] = int(action)
	}
	payload, _ := store.EncodePayload(values)
	return Source{WormID: w.ID, Team: team, BrainVersionID: w.BrainVersion, Rules: payload}
}

// Config controls one reproducible experiment.
type Config struct {
	Policy         Policy   `json:"policy"`
	Seed           int64    `json:"seed"`
	NoiseRate      float64  `json:"noise_rate,omitempty"`      // probability that an incoming rule is corrupted
	CorruptionRate float64  `json:"corruption_rate,omitempty"` // alias; the larger of the two rates is used
	Sources        []Source `json:"sources"`
}

// Rule is the normalized representation used by the conflict resolver.
type Rule struct {
	Key   string
	Value json.RawMessage
}

// RuleChange describes one observable difference from the recipient source.
type RuleChange struct {
	Key       string          `json:"key"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	DonorID   string          `json:"donor_id,omitempty"`
	Corrupted bool            `json:"corrupted,omitempty"`
}

// Provenance records every dimension which can affect a derived table.
type Provenance struct {
	Policy         Policy       `json:"policy"`
	Seed           int64        `json:"seed,omitempty"`
	NoiseRate      float64      `json:"noise_rate,omitempty"`
	CorruptionRate float64      `json:"corruption_rate,omitempty"`
	DonorIDs       []string     `json:"donor_ids,omitempty"`
	RecipientID    string       `json:"recipient_id"`
	SourceVersions []string     `json:"source_version_ids,omitempty"`
	Corruptions    []Corruption `json:"corruptions,omitempty"`
}

// Corruption is deterministic audit data for one altered incoming rule.
type Corruption struct {
	DonorID string          `json:"donor_id"`
	Key     string          `json:"key"`
	Before  json.RawMessage `json:"before"`
	After   json.RawMessage `json:"after"`
}

// Lineage describes the complete parent set (store's single parent pointer is
// insufficient for a merge, so this is persisted in the lineage payload).
type Lineage struct {
	RecipientVersionID string   `json:"recipient_version_id"`
	DonorVersionIDs    []string `json:"donor_version_ids,omitempty"`
	ParentVersionIDs   []string `json:"parent_version_ids"`
}

// Derived is one recipient's canonical derived rule table.
type Derived struct {
	Recipient  Source          `json:"recipient"`
	Rules      json.RawMessage `json:"rules"`
	Hash       string          `json:"hash"`
	Lineage    Lineage         `json:"lineage"`
	Provenance Provenance      `json:"provenance"`
	Additions  []RuleChange    `json:"additions,omitempty"`
	Removals   []RuleChange    `json:"removals,omitempty"`
	Changes    []RuleChange    `json:"changes,omitempty"`
}

// Output is a complete experiment result. Results and hashes are independent
// of wall clock time, random map iteration, and database-generated IDs.
type Output struct {
	Policy  Policy    `json:"policy"`
	Seed    int64     `json:"seed"`
	Derived []Derived `json:"derived"`
	Hash    string    `json:"hash"`
}

// BrainVersionReader is intentionally narrower than store.Store.
type BrainVersionReader interface {
	GetBrainVersion(context.Context, string) (store.BrainVersion, error)
}

// BrainVersionWriter is intentionally narrower than store.Store.
type BrainVersionWriter interface {
	CreateBrainVersion(context.Context, store.CreateBrainVersionInput) (store.BrainVersion, error)
}

// BrainVersionLister provides the current target sequence for optimistic
// persistence. Store implements this alongside BrainVersionWriter.
type BrainVersionLister interface {
	ListBrainVersions(context.Context, string, store.BrainListOptions) ([]store.BrainVersion, error)
}

func validateConfig(c Config) error {
	switch c.Policy {
	case NoSharing, SameTeamSharing, AllWormSharing, SeededNoisySharing:
	default:
		return fmt.Errorf("%w: policy %q", ErrInvalid, c.Policy)
	}
	if !finiteRate(c.NoiseRate) || !finiteRate(c.CorruptionRate) {
		return fmt.Errorf("%w: corruption rate must be finite", ErrInvalid)
	}
	if c.NoiseRate < 0 || c.NoiseRate > 1 || c.CorruptionRate < 0 || c.CorruptionRate > 1 {
		return fmt.Errorf("%w: corruption rate", ErrInvalid)
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("%w: no sources", ErrInvalid)
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.WormID == "" {
			return fmt.Errorf("%w: source %d has no worm id", ErrInvalid, i)
		}
		if seen[s.WormID] {
			return fmt.Errorf("%w: duplicate worm %q", ErrInvalid, s.WormID)
		}
		seen[s.WormID] = true
		if len(s.Rules) == 0 && s.BrainVersionID == "" {
			return fmt.Errorf("%w: source %q has no rules or version", ErrMissing, s.WormID)
		}
	}
	return nil
}

func finiteRate(rate float64) bool {
	return !math.IsNaN(rate) && !math.IsInf(rate, 0)
}

func copySources(sources []Source) []Source {
	out := make([]Source, len(sources))
	for i, source := range sources {
		out[i] = source
		out[i].Rules = append(json.RawMessage(nil), source.Rules...)
	}
	return out
}

func verifyBrainVersion(id string, v store.BrainVersion) error {
	if v.ID != id || len(v.Rules.Payload) == 0 || v.Rules.Hash == "" || stableHash(v.Rules.Payload) != v.Rules.Hash {
		return fmt.Errorf("%w: source version %q failed identity/hash verification", ErrInvalid, id)
	}
	return nil
}

// DeriveFromStore loads only the referenced immutable versions, then delegates
// to Derive. No store mutation is performed.
func DeriveFromStore(ctx context.Context, r BrainVersionReader, c Config) (Output, error) {
	if r == nil {
		return Output{}, fmt.Errorf("%w: nil reader", ErrInvalid)
	}
	if err := validateConfig(c); err != nil {
		return Output{}, err
	}
	cc := c
	cc.Sources = copySources(c.Sources)
	for i, src := range cc.Sources {
		if src.BrainVersionID == "" {
			continue
		}
		v, err := r.GetBrainVersion(ctx, src.BrainVersionID)
		if err != nil {
			return Output{}, err
		}
		if err := verifyBrainVersion(src.BrainVersionID, v); err != nil {
			return Output{}, err
		}
		loaded := append(json.RawMessage(nil), v.Rules.Payload...)
		if len(src.Rules) != 0 && !bytes.Equal(canonicalJSON(src.Rules), canonicalJSON(loaded)) {
			return Output{}, fmt.Errorf("%w: source %q rules do not match version %q", ErrInvalid, src.WormID, src.BrainVersionID)
		}
		cc.Sources[i].Rules = loaded
	}
	return Derive(cc)
}

// Derive computes a reproducible result from source payloads.
func Derive(c Config) (Output, error) {
	if err := validateConfig(c); err != nil {
		return Output{}, err
	}
	sources := copySources(c.Sources)
	sort.Slice(sources, func(i, j int) bool { return sources[i].WormID < sources[j].WormID })
	parsed := make(map[string][]Rule, len(sources))
	for _, src := range sources {
		rules, err := decodeRules(src.Rules)
		if err != nil {
			return Output{}, fmt.Errorf("%w: %s: %v", ErrInvalid, src.WormID, err)
		}
		parsed[src.WormID] = rules
	}
	out := Output{Policy: c.Policy, Seed: c.Seed}
	for _, recipient := range sources {
		rules := append([]Rule(nil), parsed[recipient.WormID]...)
		base := ruleMap(rules)
		owners := make(map[string]string, len(base))
		corruptedKeys := map[string]bool{}
		for key := range base {
			owners[key] = recipient.WormID
		}
		candidates := make([]Source, 0, len(sources))
		for _, donor := range sources {
			if donor.WormID == recipient.WormID {
				continue
			}
			switch c.Policy {
			case NoSharing:
			case SameTeamSharing:
				if donor.Team != "" && donor.Team == recipient.Team {
					candidates = append(candidates, donor)
				}
			case AllWormSharing, SeededNoisySharing:
				candidates = append(candidates, donor)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].WormID != candidates[j].WormID {
				return candidates[i].WormID < candidates[j].WormID
			}
			return candidates[i].BrainVersionID < candidates[j].BrainVersionID
		})
		prov := Provenance{Policy: c.Policy, Seed: c.Seed, NoiseRate: c.NoiseRate, CorruptionRate: c.CorruptionRate, RecipientID: recipient.WormID, SourceVersions: []string{recipient.BrainVersionID}}
		lineage := Lineage{RecipientVersionID: recipient.BrainVersionID, ParentVersionIDs: []string{recipient.BrainVersionID}}
		for _, donor := range candidates {
			prov.DonorIDs = append(prov.DonorIDs, donor.WormID)
			if donor.BrainVersionID != "" {
				prov.SourceVersions = append(prov.SourceVersions, donor.BrainVersionID)
				lineage.DonorVersionIDs = append(lineage.DonorVersionIDs, donor.BrainVersionID)
				lineage.ParentVersionIDs = append(lineage.ParentVersionIDs, donor.BrainVersionID)
			}
			for _, rule := range parsed[donor.WormID] {
				value := append(json.RawMessage(nil), rule.Value...)
				wasCorrupted := false
				if c.Policy == SeededNoisySharing && corruptionDecision(c.Seed, recipient.WormID, donor.WormID, rule.Key, maxRate(c.NoiseRate, c.CorruptionRate)) {
					corrupted := corrupt(value)
					prov.Corruptions = append(prov.Corruptions, Corruption{
						DonorID: donor.WormID, Key: rule.Key,
						Before: append(json.RawMessage(nil), rule.Value...),
						After:  append(json.RawMessage(nil), corrupted...),
					})
					wasCorrupted = true
					value = corrupted
				}
				owner, exists := owners[rule.Key]
				// Recipient rules are a fallback. Among conflicting donors,
				// the lexicographically first donor wins because candidates
				// are sorted above.
				if exists && owner != recipient.WormID {
					continue
				}
				base[rule.Key] = Rule{Key: rule.Key, Value: value}
				if wasCorrupted {
					corruptedKeys[rule.Key] = true
				}
				owners[rule.Key] = donor.WormID
			}
		}
		merged := sortedRules(base)
		canonical, err := encodeRules(merged)
		if err != nil {
			return Output{}, err
		}
		sourceCanonical, err := encodeRules(parsed[recipient.WormID])
		if err != nil {
			return Output{}, err
		}
		canonicalRecipient := recipient
		canonicalRecipient.Rules = sourceCanonical
		d := Derived{Recipient: canonicalRecipient, Rules: canonical, Hash: stableHash(canonical), Lineage: lineage, Provenance: prov}
		before := ruleMap(parsed[recipient.WormID])
		for _, r := range merged {
			old, ok := before[r.Key]
			if !ok {
				d.Additions = append(d.Additions, RuleChange{Key: r.Key, After: append(json.RawMessage(nil), r.Value...), DonorID: owners[r.Key], Corrupted: corruptedKeys[r.Key]})
			} else if string(old.Value) != string(r.Value) {
				d.Changes = append(d.Changes, RuleChange{Key: r.Key, Before: append(json.RawMessage(nil), old.Value...), After: append(json.RawMessage(nil), r.Value...), DonorID: owners[r.Key], Corrupted: corruptedKeys[r.Key]})
			}
		}
		for _, r := range parsed[recipient.WormID] {
			if _, ok := base[r.Key]; !ok {

				d.Removals = append(d.Removals, RuleChange{Key: r.Key, Before: append(json.RawMessage(nil), r.Value...)})
			}
		}
		sortChanges(d.Additions)
		sortChanges(d.Removals)
		sortChanges(d.Changes)
		out.Derived = append(out.Derived, d)
	}
	output, err := outputBytes(out)
	if err != nil {
		return Output{}, err
	}
	out.Hash = stableHash(output)
	return out, nil
}

// Run is a concise alias for Derive.
func Run(c Config) (Output, error) { return Derive(c) }

// RunFromStore is a concise alias for DeriveFromStore.
func RunFromStore(ctx context.Context, r BrainVersionReader, c Config) (Output, error) {
	return DeriveFromStore(ctx, r, c)
}

func maxRate(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func stableHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func corruptionDecision(seed int64, recipient, donor, key string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	h := sha256.New()
	var seedBytes [8]byte
	binary.BigEndian.PutUint64(seedBytes[:], uint64(seed))
	_, _ = h.Write(seedBytes[:])
	for _, part := range []string{recipient, donor, key} {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(part))
	}
	sum := h.Sum(nil)
	draw := binary.BigEndian.Uint64(sum[:8])
	return float64(draw)/float64(^uint64(0)) < rate
}

func outputBytes(o Output) ([]byte, error) {
	x := struct {
		Policy  Policy    `json:"policy"`
		Seed    int64     `json:"seed"`
		Derived []Derived `json:"derived"`
	}{o.Policy, o.Seed, o.Derived}
	return json.Marshal(x)
}
func ruleMap(rs []Rule) map[string]Rule {
	out := make(map[string]Rule, len(rs))
	for _, r := range rs {
		if _, ok := out[r.Key]; !ok {
			out[r.Key] = r
		}
	}
	return out
}
func sortedRules(m map[string]Rule) []Rule {
	out := make([]Rule, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
func sortChanges(x []RuleChange) { sort.Slice(x, func(i, j int) bool { return x[i].Key < x[j].Key }) }
func origin(key string, recipient Source, ds []Source, parsed map[string][]Rule) string {
	for _, d := range ds {
		for _, r := range parsed[d.WormID] {
			if r.Key == key {
				return d.WormID
			}
		}
	}
	return ""
}

func decodeRules(raw json.RawMessage) ([]Rule, error) {
	var env struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Data != nil {
		raw = env.Data
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		out := make([]Rule, 0, len(arr))
		allExplicit := len(arr) > 0
		for i, value := range arr {
			var entry struct {
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			}
			explicit := json.Unmarshal(value, &entry) == nil && entry.Key != "" && entry.Value != nil
			if explicit {
				out = append(out, Rule{Key: entry.Key, Value: canonicalJSON(entry.Value)})
			} else {
				allExplicit = false
				out = append(out, Rule{Key: strconv.Itoa(i), Value: canonicalJSON(value)})
			}
		}
		if allExplicit {
			sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		}
		return uniqueRules(out)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, errors.New("rules are not an array or object")
	}
	for _, key := range []string{"rules", "rule_table", "entries"} {
		if value, ok := obj[key]; ok {
			rules, err := decodeRules(value)
			if err != nil {
				return nil, err
			}
			return rules, nil
		}
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Rule, 0, len(keys))
	for _, key := range keys {
		out = append(out, Rule{Key: key, Value: canonicalJSON(obj[key])})
	}
	return uniqueRules(out)
}

func uniqueRules(rs []Rule) ([]Rule, error) {
	seen := make(map[string]struct{}, len(rs))
	for _, rule := range rs {
		if _, exists := seen[rule.Key]; exists {
			return nil, fmt.Errorf("duplicate normalized rule key %q", rule.Key)
		}
		seen[rule.Key] = struct{}{}
	}
	return rs, nil
}
func canonicalJSON(raw []byte) json.RawMessage {
	var x any
	if json.Unmarshal(raw, &x) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	b, _ := json.Marshal(x)
	return b
}
func encodeRules(rs []Rule) (json.RawMessage, error) {
	// Numeric mask tables retain the engine/debug canonical array form.
	// Non-numeric libraries use explicit key/value entries.
	numeric := make(map[int]json.RawMessage, len(rs))
	for _, r := range rs {
		n, err := strconv.Atoi(r.Key)
		if err != nil || n < 0 {
			numeric = nil
			break
		}
		if _, exists := numeric[n]; exists {
			numeric = nil
			break
		}
		numeric[n] = canonicalJSON(r.Value)
	}
	if numeric != nil {
		values := make([]json.RawMessage, len(rs))
		for i := range values {
			value, ok := numeric[i]
			if !ok {
				numeric = nil
				break
			}
			values[i] = value
		}
		if numeric != nil {
			return store.EncodePayload(values)
		}
	}
	arr := make([]struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}, len(rs))
	for i, r := range rs {
		arr[i] = struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}{r.Key, canonicalJSON(r.Value)}
	}
	return store.EncodePayload(arr)
}
func corrupt(v json.RawMessage) json.RawMessage {
	var n int
	if json.Unmarshal(v, &n) == nil {
		return json.RawMessage(strconv.Itoa((n + 1 + 6) % 6))
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		b, _ := json.Marshal("corrupt:" + s)
		return b
	}
	var b bool
	if json.Unmarshal(v, &b) == nil {
		if b {
			return json.RawMessage("false")
		}
		return json.RawMessage("true")
	}
	return json.RawMessage(`null`)
}

// Persist writes each derived table through the narrow writer interface. The
// target brain must already exist; no source is updated or deleted. When the
// writer exposes BrainVersionLister, each record is appended after the target's
// current version and the database's unique sequence check rejects races.
func (o Output) Persist(ctx context.Context, w BrainVersionWriter, targetBrainID string) ([]store.BrainVersion, error) {
	if w == nil || targetBrainID == "" {
		return nil, fmt.Errorf("%w: persistence target", ErrInvalid)
	}
	var current store.BrainVersion
	haveCurrent := false
	hasLister := false
	if lister, ok := w.(BrainVersionLister); ok {
		hasLister = true
		var err error
		current, haveCurrent, err = latestBrainVersion(ctx, lister, targetBrainID)
		if err != nil {
			return nil, err
		}
	}
	out := make([]store.BrainVersion, 0, len(o.Derived))
	for _, d := range o.Derived {
		lineage, err := store.EncodePayload(d.Lineage)
		if err != nil {
			return out, err
		}
		prov, err := store.EncodePayload(d.Provenance)
		if err != nil {
			return out, err
		}
		payload, err := store.EncodePayload(struct {
			Policy    Policy       `json:"policy"`
			Recipient string       `json:"recipient"`
			Hash      string       `json:"hash"`
			Additions []RuleChange `json:"additions,omitempty"`
			Removals  []RuleChange `json:"removals,omitempty"`
			Changes   []RuleChange `json:"changes,omitempty"`
		}{o.Policy, d.Recipient.WormID, d.Hash, d.Additions, d.Removals, d.Changes})
		if err != nil {
			return out, err
		}
		id := "sharing-" + stableHash([]byte(d.Recipient.WormID+"\x00"+d.Hash))
		input := store.CreateBrainVersionInput{
			ID: id, BrainID: targetBrainID, Version: int64(len(out) + 1),
			Rules:      append(json.RawMessage(nil), d.Rules...),
			Lineage:    append(json.RawMessage(nil), lineage...),
			Provenance: append(json.RawMessage(nil), prov...),
			Payload:    append(json.RawMessage(nil), payload...),
		}
		if haveCurrent {
			input.Version = current.Version + 1
			input.ParentVersionID = current.ID
		} else if hasLister {
			input.Version = 1
		}
		v, err := w.CreateBrainVersion(ctx, input)
		if err != nil {
			return out, err
		}
		out = append(out, v)
		current, haveCurrent = v, true
	}
	return out, nil
}

func latestBrainVersion(ctx context.Context, lister BrainVersionLister, brainID string) (store.BrainVersion, bool, error) {
	var latest store.BrainVersion
	found := false
	for offset := 0; ; offset += 1000 {
		page, err := lister.ListBrainVersions(ctx, brainID, store.BrainListOptions{Limit: 1000, Offset: offset})
		if err != nil {
			return store.BrainVersion{}, false, err
		}
		for _, version := range page {
			if !found || version.Version > latest.Version {
				latest, found = version, true
			}
		}
		if len(page) < 1000 {
			break
		}
	}
	return latest, found, nil
}

// Persist is the package-level form of Output.Persist.
func Persist(ctx context.Context, w BrainVersionWriter, targetBrainID string, o Output) ([]store.BrainVersion, error) {
	return o.Persist(ctx, w, targetBrainID)
}

// Observation is one completed tournament/match observation. Sharing does not
// import the tournament package, keeping experiment evaluation reusable.
type Observation struct {
	Policy          Policy
	Score           float64
	Survived        bool
	KnownPatterns   int
	UnknownPatterns int
}
type Metrics struct {
	Score              float64 `json:"score"`
	Survival           float64 `json:"survival"`
	Coverage           float64 `json:"coverage"`
	UnknownPatternRate float64 `json:"unknown_pattern_rate"`
	Observations       int     `json:"observations"`
}
type Comparison struct {
	Policies map[Policy]Metrics `json:"policies"`
}

func AggregateMetrics(xs []Observation) Metrics {
	var m Metrics
	m.Observations = len(xs)
	if len(xs) == 0 {
		return m
	}
	known, unknown := 0, 0
	for _, x := range xs {
		m.Score += x.Score
		if x.Survived {
			m.Survival++
		}
		known += x.KnownPatterns
		unknown += x.UnknownPatterns
	}
	n := float64(len(xs))
	m.Score /= n
	m.Survival /= n
	if total := known + unknown; total > 0 {
		m.Coverage = float64(known) / float64(total)
		m.UnknownPatternRate = float64(unknown) / float64(total)
	}
	return m
}
func CompareMetrics(xs []Observation) Comparison {
	c := Comparison{Policies: map[Policy]Metrics{}}
	groups := map[Policy][]Observation{}
	for _, x := range xs {
		groups[x.Policy] = append(groups[x.Policy], x)
	}
	keys := make([]string, 0, len(groups))
	for p := range groups {
		keys = append(keys, string(p))
	}
	sort.Strings(keys)
	for _, p := range keys {
		c.Policies[Policy(p)] = AggregateMetrics(groups[Policy(p)])
	}
	return c
}

// CanonicalHash computes the stable hash used for derived rule tables.
func CanonicalHash(rules json.RawMessage) string { return stableHash(canonicalJSON(rules)) }
