package debug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	ExitOK         = 0
	ExitInvalid    = 2
	ExitNotFound   = 3
	ExitConnection = 4
	ExitSchema     = 5
	ExitCorruption = 6
)

type cliOptions struct {
	db, api, out                                                      string
	redact, jsonOutput, help, authorizeImport                         bool
	version, pattern, status, brain, stopBrain, stopPattern, from, to string
	until, limit, offset                                              int64
	rules, provenance, games, decisions, captures, deaths             bool
}

const Usage = `wormsctl [--db FILE|--api URL] [--json|--human] COMMAND
commands:
  brain show ID [--version ID|NUMBER] [--pattern TEXT] [--limit N] [--offset N]
  brain diff FROM TO
  game list [--status STATUS] [--brain VERSION]
  game replay ID [--seek N] [--pattern TEXT] [--decisions|--captures|--deaths]
  game verify ID
  diagnostic export [BRAIN-ID] [GAME-ID] [--redact]
  diagnostic import FILE [--out FILE] [--db EMPTY.db --authorize-import]

Diagnostic import is non-destructive by default. Database restore requires the
explicit --authorize-import flag and only accepts a nonexistent or empty file.`

func parseCLI(args []string) ([]string, cliOptions, error) {
	o := cliOptions{db: os.Getenv("WORMS_DB"), api: os.Getenv("WORMS_API_URL"), out: "-", jsonOutput: true}
	var pos []string
	takes := map[string]bool{"db": true, "api": true, "out": true, "version": true, "pattern": true, "status": true, "brain": true, "stop-on-brain": true, "stop-on-pattern": true, "seek": true, "from": true, "to": true, "limit": true, "offset": true}
	bools := map[string]bool{"redact": true, "json": true, "human": true, "help": true, "authorize-import": true, "allow-write": true, "allow-restore": true, "restore": true, "rules": true, "provenance": true, "games": true, "decisions": true, "captures": true, "deaths": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		a = strings.TrimPrefix(a, "--")
		k, v, has := strings.Cut(a, "=")
		if bools[k] {
			if has {
				if v != "true" && v != "false" {
					return nil, o, fmt.Errorf("%w: --%s", ErrInvalid, k)
				}
				b, _ := strconv.ParseBool(v)
				switch k {
				case "redact":
					o.redact = b
				case "json":
					o.jsonOutput = b
				case "human":
					o.jsonOutput = !b
				case "help":
					o.help = b
				case "authorize-import", "allow-write", "allow-restore", "restore":
					o.authorizeImport = b
				case "rules":
					o.rules = b
				case "provenance":
					o.provenance = b
				case "games":
					o.games = b
				case "decisions":
					o.decisions = b
				case "captures":
					o.captures = b
				case "deaths":
					o.deaths = b
				}
			} else {
				switch k {
				case "help":
					o.help = true
				case "authorize-import", "allow-write", "allow-restore", "restore":
					o.authorizeImport = true
				case "human":
					o.jsonOutput = false
				case "redact":
					o.redact = true
				case "json":
					o.jsonOutput = true
				case "rules":
					o.rules = true
				case "provenance":
					o.provenance = true
				case "games":
					o.games = true
				case "decisions":
					o.decisions = true
				case "captures":
					o.captures = true
				case "deaths":
					o.deaths = true
				}
			}
			continue
		}
		if !takes[k] {
			return nil, o, fmt.Errorf("%w: unknown option --%s", ErrInvalid, k)
		}
		if !has {
			if i+1 >= len(args) {
				return nil, o, fmt.Errorf("%w: --%s needs value", ErrInvalid, k)
			}
			i++
			v = args[i]
		}
		switch k {
		case "db":
			o.db = v
		case "api":
			o.api = v
		case "out":
			o.out = v
		case "version":
			o.version = v
		case "pattern":
			o.pattern = v
		case "status":
			o.status = v
		case "brain":
			o.brain = v
		case "stop-on-brain":
			o.stopBrain = v
		case "stop-on-pattern":
			o.stopPattern = v
		case "from":
			o.from = v
		case "to":
			o.to = v
		case "seek":
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 0 {
				return nil, o, fmt.Errorf("%w: seek", ErrInvalid)
			}
			o.until = n
		case "limit", "offset":
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 0 || (k == "limit" && n == 0) {
				return nil, o, fmt.Errorf("%w: --%s", ErrInvalid, k)
			}
			if k == "limit" {
				o.limit = n
			} else {
				o.offset = n
			}
		}
	}
	return pos, o, nil
}
func writeVersioned(w io.Writer, v any) error {
	raw, e := Versioned(v)
	if e != nil {
		return e
	}
	if o, ok := w.(interface{ Write([]byte) (int, error) }); ok {
		_, e = o.Write(append(raw, '\n'))
		return e
	}
	return nil
}
func Run(ctx context.Context, args []string, out, errout io.Writer) int {
	pos, o, e := parseCLI(args)
	if e != nil {
		return reportError(errout, e, o)
	}
	if o.help {
		if o.jsonOutput {
			_ = writeVersioned(out, map[string]string{"usage": Usage})
		} else if out != nil {
			fmt.Fprintln(out, Usage)
		}
		return ExitOK
	}
	if len(pos) < 2 {
		return reportError(errout, fmt.Errorf("%w: command (use --help)", ErrInvalid), o)
	}
	cmd, sub := pos[0], pos[1]
	if cmd == "diagnostic" && (sub == "import" || sub == "export") {
		if sub == "import" {
			return runImport(o, pos[2:], out, errout)
		}
		return runWithReader(ctx, o, pos[2:], func(r Reader) error { return runExport(ctx, r, o, pos[2:], out) }, errout)
	}
	return runWithReader(ctx, o, pos[2:], func(r Reader) error { return runCommand(ctx, r, cmd, sub, pos[2:], o, out) }, errout)
}
func reportError(w io.Writer, e error, options ...cliOptions) int {
	code := exitCode(e)
	if w != nil {
		jsonOutput := len(options) > 0 && options[0].jsonOutput
		if jsonOutput {
			_ = writeVersioned(w, map[string]any{"error": e.Error(), "code": code})
		} else {
			fmt.Fprintln(w, e)
		}
	}
	return code
}
func exitCode(e error) int {
	switch {
	case errors.Is(e, ErrInvalid):
		return ExitInvalid
	case errors.Is(e, ErrNotFound), errors.Is(e, ErrEmptyBrain):
		return ExitNotFound
	case errors.Is(e, ErrSchema):
		return ExitSchema
	case errors.Is(e, ErrCorrupt), errors.Is(e, ErrDivergence):
		return ExitCorruption
	default:
		return ExitConnection
	}
}
func runWithReader(ctx context.Context, o cliOptions, rest []string, fn func(Reader) error, errout io.Writer) int {
	if o.db != "" && o.api != "" {
		return reportError(errout, fmt.Errorf("%w: choose --db or --api", ErrInvalid), o)
	}
	var r Reader
	var e error
	if o.api != "" {
		r = NewAPIReader(o.api)
	} else {
		r, e = OpenSQLite(ctx, o.db)
		if e != nil {
			return reportError(errout, e, o)
		}
	}
	defer r.Close()
	if e = fn(r); e != nil {
		return reportError(errout, e, o)
	}
	return ExitOK
}
func runCommand(ctx context.Context, r Reader, cmd, sub string, args []string, o cliOptions, out io.Writer) error {
	switch cmd {
	case "brain":
		switch sub {
		case "show":
			if len(args) != 1 {
				return fmt.Errorf("%w: brain show id", ErrInvalid)
			}
			return runBrainShow(ctx, r, args[0], o, out)
		case "diff":
			a, b := "", ""
			if len(args) == 2 {
				a, b = args[0], args[1]
			}
			if len(args) == 1 && o.from != "" && o.to != "" {
				a, b = o.from, o.to
			}
			if a == "" || b == "" {
				return fmt.Errorf("%w: brain diff needs two version ids or --from/--to", ErrInvalid)
			}
			d, err := r.Diff(ctx, a, b)
			if err != nil {
				return err
			}
			if o.jsonOutput {
				return writeVersioned(out, d)
			}
			fmt.Fprintf(out, "Brain diff %s -> %s\nrules_changed=%t lineage_changed=%t provenance_changed=%t payload_changed=%t\n", a, b, d.RulesChanged, d.LineageChanged, d.ProvenanceChanged, d.PayloadChanged)
			return nil
		}
	case "game":
		switch sub {
		case "replay", "seek", "verify":
		case "list":
			if len(args) > 0 {
				return fmt.Errorf("%w: game list takes no id", ErrInvalid)
			}
			return runGameList(ctx, r, o, out)
		default:
			return fmt.Errorf("%w: unknown game command", ErrInvalid)
		}
		if len(args) != 1 {
			return fmt.Errorf("%w: game %s id", ErrInvalid, sub)
		}
		if sub == "verify" {
			g, err := r.Game(ctx, args[0])
			if err != nil {
				return err
			}
			es, err := r.Events(ctx, args[0], 0)
			if err != nil {
				return err
			}
			if err = VerifyEvents(g, es); err != nil {
				return err
			}
			return writeVersioned(out, map[string]any{"game_id": args[0], "valid": true, "sequence": len(es)})
		}
		res, err := Replay(ctx, r, args[0], ReplayOptions{Until: o.until, Pattern: o.pattern, StopOnBrain: o.stopBrain, StopOnPattern: o.stopPattern})
		if err != nil {
			return err
		}
		return writeVersioned(out, res)
	default:
		return fmt.Errorf("%w: unknown command", ErrInvalid)
	}
	return fmt.Errorf("%w: unknown brain command", ErrInvalid)
}
func runBrainShow(ctx context.Context, r Reader, id string, o cliOptions, out io.Writer) error {
	var (
		b        BrainInspection
		page     BrainPage
		versions []BrainVersion
		err      error
	)
	// A requested page is always fetched from the backing reader. This is
	// important for large brains: neither SQLite nor the live API may hydrate
	// every historical rule table just to discard most of it in the CLI.
	if o.version == "" && (o.limit > 0 || o.offset > 0) {
		limit := o.limit
		if limit == 0 {
			limit = 50
		}
		page, err = r.BrainPage(ctx, id, int(limit), int(o.offset))
		if err != nil {
			return err
		}
		b = BrainInspection{Brain: page.Brain, Versions: page.Versions}
		versions = page.Versions
	} else {
		b, err = r.Brain(ctx, id)
		if err != nil {
			return err
		}
		versions = b.Versions
		if o.version != "" {
			selected := versions[:0]
			for _, v := range versions {
				if strconv.FormatInt(v.Version, 10) == o.version || v.ID == o.version {
					selected = append(selected, v)
				}
			}
			if len(selected) == 0 {
				return fmt.Errorf("%w: version %s", ErrNotFound, o.version)
			}
			versions = selected
		}
		if o.offset > int64(len(versions)) {
			versions = nil
		} else if o.offset > 0 {
			versions = versions[o.offset:]
		}
		if o.limit > 0 && int64(len(versions)) > o.limit {
			versions = versions[:o.limit]
		}
	}
	if o.pattern != "" {
		filtered := versions[:0]
		pattern := strings.ToLower(o.pattern)
		for _, v := range versions {
			if strings.Contains(strings.ToLower(string(v.Payload)), pattern) ||
				strings.Contains(strings.ToLower(string(v.Rules.Payload)), pattern) ||
				strings.Contains(strings.ToLower(string(v.Provenance.Payload)), pattern) {
				filtered = append(filtered, v)
			}
		}
		versions = filtered
	}
	payload := map[string]any{"brain": b.Brain, "versions": versions}
	if page.Total != 0 {
		payload["total"] = page.Total
		payload["offset"] = page.Offset
		payload["limit"] = page.Limit
		if page.NextOffset != 0 {
			payload["next_offset"] = page.NextOffset
		}
	}
	if !o.rules {
		for i := range versions {
			versions[i].Rules = Component{}
		}
	}
	if !o.provenance {
		for i := range versions {
			versions[i].Provenance = Component{}
		}
	}
	if o.games {
		var gs []Game
		seen := map[string]bool{}
		for _, v := range versions {
			x, e := r.Games(ctx, v.ID)
			if e != nil {
				return e
			}
			for _, g := range x {
				if (g.BrainVersionID == "" || g.BrainVersionID == v.ID) && !seen[g.ID] {
					seen[g.ID] = true
					gs = append(gs, g)
				}
			}
		}
		payload["games"] = gs
	}
	if !o.jsonOutput {
		return writeHumanBrain(out, b.Brain, versions)
	}
	payload["versions"] = versions
	return writeVersioned(out, payload)
}
func writeHumanBrain(out io.Writer, b Brain, versions []BrainVersion) error {
	fmt.Fprintf(out, "Brain %s (%s)\n", b.ID, b.Name)
	for _, v := range versions {
		fmt.Fprintf(out, "Version %d id=%s hash=%s\n", v.Version, v.ID, v.Hash)
		if v.Lineage.ParentVersionID != "" {
			fmt.Fprintf(out, "  lineage parent=%s\n", v.Lineage.ParentVersionID)
		}
		for _, ref := range v.References {
			lower := strings.ToLower(ref)
			if strings.Contains(lower, "event") || strings.Contains(lower, "learned") || strings.Contains(lower, "source") || strings.Contains(lower, "game") {
				fmt.Fprintf(out, "  provenance %s\n", ref)
			} else {
				fmt.Fprintf(out, "  reference %s\n", ref)
			}
		}
		for _, use := range v.Usage {
			fmt.Fprintf(out, "  rule_usage %s\n", use)
		}
		rules := v.RulesDecoded
		if len(rules) == 0 {
			rules, _ = DecodeRules(v.Rules.Payload)
		}
		for _, rule := range rules {
			fmt.Fprintf(out, "  mask=%02x pattern=%s action=%s diagram=%s\n", rule.Mask, rule.Pattern, rule.Action, rule.Diagram)
		}
	}
	return nil
}
func runGameList(ctx context.Context, r Reader, o cliOptions, out io.Writer) error {
	gs, e := r.Games(ctx, o.brain)
	if e != nil {
		return e
	}
	if o.brain != "" {
		x := gs[:0]
		for _, g := range gs {
			matched := g.BrainVersionID == o.brain
			for _, p := range g.Participants {
				if p.BrainVersionID == o.brain {
					matched = true
					break
				}
			}
			if matched {
				x = append(x, g)
			}
		}
		gs = x
	}
	if o.status != "" {
		x := gs[:0]
		for _, g := range gs {
			if g.Status == o.status {
				x = append(x, g)
			}
		}
		gs = x
	}
	if o.decisions || o.captures || o.deaths || o.pattern != "" {
		reports := []ReplayResult{}
		for _, g := range gs {
			res, e := Replay(ctx, r, g.ID, ReplayOptions{Pattern: o.pattern})
			if e != nil {
				return e
			}
			if o.decisions || o.captures || o.deaths {
				selected := make([]Event, 0, len(res.Events))
				for _, ev := range res.Events {
					isDecision := strings.Contains(strings.ToLower(ev.Type), "decision")
					isCapture := strings.Contains(strings.ToLower(ev.Type), "captur")
					isDeath := strings.Contains(strings.ToLower(ev.Type), "death")
					if (o.decisions && isDecision) || (o.captures && isCapture) || (o.deaths && isDeath) {
						selected = append(selected, ev)
					}
				}
				res.Events = selected
			}
			reports = append(reports, res)
		}
		return writeVersioned(out, reports)
	}
	return writeVersioned(out, gs)
}
func runExport(ctx context.Context, r Reader, o cliOptions, args []string, out io.Writer) error {
	if len(args) > 2 {
		return fmt.Errorf("%w: diagnostic export [brain-id] [game-id]", ErrInvalid)
	}
	brain, game := "", ""
	if len(args) > 0 {
		brain = args[0]
	}
	if len(args) > 1 {
		game = args[1]
	}
	d, e := Export(ctx, r, brain, game, o.redact)
	if e != nil {
		return e
	}
	var w io.Writer = out
	if o.out != "-" {
		f, e := os.Create(o.out)
		if e != nil {
			return fmt.Errorf("%w: %v", ErrConnection, e)
		}
		defer f.Close()
		w = f
	}
	return WriteDiagnostic(w, d)
}
func runImport(o cliOptions, args []string, out, errout io.Writer) int {
	if len(args) != 1 {
		return reportError(errout, fmt.Errorf("%w: diagnostic import file", ErrInvalid), o)
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return reportError(errout, fmt.Errorf("%w: %v", ErrConnection, err), o)
	}
	d, err := ImportDiagnostic(raw)
	if err != nil {
		return reportError(errout, err, o)
	}
	if o.db != "" {
		if o.api != "" {
			return reportError(errout, fmt.Errorf("%w: choose --db or --api", ErrInvalid), o)
		}
		if !o.authorizeImport {
			return reportError(errout, fmt.Errorf("%w: diagnostic database restore requires --authorize-import", ErrInvalid), o)
		}
		if err = RestoreDiagnostic(context.Background(), o.db, d); err != nil {
			return reportError(errout, err, o)
		}
		if out != nil {
			_ = writeVersioned(out, map[string]any{"restored": true, "database": o.db, "hashes": d.Hashes})
		}
		return ExitOK
	}
	if o.out != "-" {
		f, err := os.Create(o.out)
		if err != nil {
			return reportError(errout, fmt.Errorf("%w: %v", ErrConnection, err), o)
		}
		defer f.Close()
		err = WriteDiagnostic(f, d)
	} else {
		err = WriteDiagnostic(out, d)
	}
	if err != nil {
		return reportError(errout, err, o)
	}
	return ExitOK
}

var _ = json.Valid
