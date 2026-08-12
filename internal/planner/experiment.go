package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"worms.ng/internal/engine"
)

var ErrSeedLeakage = errors.New("planner: training and held-out seeds overlap")

// HeldOutConfig separates the state-generation and evaluation phases. The
// planner is passed only to Train; Evaluate receives a fresh state and cannot
// accidentally teach it through this API.
type HeldOutConfig struct {
	Planner       Config
	TrainingSeeds []int64
	HeldOutSeeds  []int64
	NewState      func(seed int64) (engine.State, error)
	Train         func(context.Context, *Planner, *engine.State, int64) error
	Evaluate      func(context.Context, engine.State, int64) (SeedResult, error)
}

type SeedResult struct {
	Seed      int64          `json:"seed"`
	StateHash string         `json:"state_hash"`
	Score     map[string]int `json:"score,omitempty"`
	Error     string         `json:"error,omitempty"`
}

type HeldOutResult struct {
	Training []SeedResult `json:"training"`
	HeldOut  []SeedResult `json:"held_out"`
}

// NewHeldOut validates partitioning before any state is generated.
func NewHeldOut(training, heldout []int64) (HeldOutConfig, error) {
	if err := ValidateSeedPartitions(training, heldout); err != nil {
		return HeldOutConfig{}, err
	}
	return HeldOutConfig{TrainingSeeds: append([]int64(nil), training...), HeldOutSeeds: append([]int64(nil), heldout...)}, nil
}

func ValidateSeedPartitions(training, heldout []int64) error {
	seenTrain := map[int64]bool{}
	for _, seed := range training {
		if seenTrain[seed] {
			return fmt.Errorf("planner: duplicate training seed %d", seed)
		}
		seenTrain[seed] = true
	}
	seenTest := map[int64]bool{}
	for _, seed := range heldout {
		if seenTest[seed] {
			return fmt.Errorf("planner: duplicate held-out seed %d", seed)
		}
		seenTest[seed] = true
		if seenTrain[seed] {
			return fmt.Errorf("%w: seed %d", ErrSeedLeakage, seed)
		}
	}
	return nil
}

// RunHeldOut trains only on TrainingSeeds and evaluates only on fresh states
// generated from HeldOutSeeds. A held-out state never enters Train and the
// planner is not consulted during Evaluate.
func RunHeldOut(ctx context.Context, cfg HeldOutConfig) (HeldOutResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ValidateSeedPartitions(cfg.TrainingSeeds, cfg.HeldOutSeeds); err != nil {
		return HeldOutResult{}, err
	}
	if cfg.NewState == nil {
		return HeldOutResult{}, errors.New("planner: NewState is required")
	}
	p, err := New(cfg.Planner)
	if err != nil {
		return HeldOutResult{}, err
	}
	out := HeldOutResult{}
	for _, seed := range cfg.TrainingSeeds {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		s, err := cfg.NewState(seed)
		if err != nil {
			return out, fmt.Errorf("planner: training seed %d: %w", seed, err)
		}
		s = s.Snapshot()
		if cfg.Train != nil {
			if err := cfg.Train(ctx, p, &s, seed); err != nil {
				return out, fmt.Errorf("planner: training seed %d: %w", seed, err)
			}
		}
		out.Training = append(out.Training, SeedResult{Seed: seed, StateHash: s.HashHex(), Score: scores(s)})
	}
	for _, seed := range cfg.HeldOutSeeds {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		s, err := cfg.NewState(seed)
		if err != nil {
			return out, fmt.Errorf("planner: held-out seed %d: %w", seed, err)
		}
		s = s.Snapshot()
		for i := range s.Worms {
			s.Worms[i].Frozen = true
		}
		ruleHash := ruleTableHash(s)
		var result SeedResult
		if cfg.Evaluate == nil {
			result = SeedResult{Seed: seed, StateHash: s.HashHex(), Score: scores(s)}
		} else {
			result, err = cfg.Evaluate(ctx, s, seed)
			if err != nil {
				return out, fmt.Errorf("planner: held-out seed %d: %w", seed, err)
			}
			if got := ruleTableHash(s); got != ruleHash {
				return out, fmt.Errorf("planner: held-out seed %d changed frozen rule tables", seed)
			}
			result.Seed = seed
			result.Score = cloneScores(result.Score)
		}
		out.HeldOut = append(out.HeldOut, result)
	}
	return out, nil
}

type ruleTableRecord struct {
	ID    string            `json:"id"`
	Rules [64]engine.Action `json:"rules"`
}

func ruleTableHash(s engine.State) string {
	rules := make([]ruleTableRecord, 0, len(s.Worms))
	for _, w := range s.Worms {
		rules = append(rules, ruleTableRecord{ID: w.ID, Rules: w.Rules})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	b, _ := json.Marshal(rules)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func cloneScores(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// EvaluateHeldOut is a concise alias for RunHeldOut.
func EvaluateHeldOut(ctx context.Context, cfg HeldOutConfig) (HeldOutResult, error) {
	return RunHeldOut(ctx, cfg)
}

func scores(s engine.State) map[string]int {
	out := map[string]int{}
	for _, w := range s.Worms {
		out[w.ID] = w.Score
	}
	return out
}

// Seeds returns a sorted copy useful for experiment manifests.
func (c HeldOutConfig) Seeds() (training, heldout []int64) {
	training = append([]int64(nil), c.TrainingSeeds...)
	heldout = append([]int64(nil), c.HeldOutSeeds...)
	sort.Slice(training, func(i, j int) bool { return training[i] < training[j] })
	sort.Slice(heldout, func(i, j int) bool { return heldout[i] < heldout[j] })
	return training, heldout
}
