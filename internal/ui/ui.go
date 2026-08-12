// Package ui contains the cross-platform Gio client shell.
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/font/gofont"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const (
	navRefresh = iota
	navPlay
	navGames
	navBrains
	navTournament
	navExperiments
	navButtonCount
)

const (
	focusNavPlay        = "nav.play"
	focusNavGames       = "nav.games"
	focusNavBrains      = "nav.brains"
	focusNavTournament  = "nav.tournament"
	focusNavExperiments = "nav.experiments"
	focusNavHealth      = "nav.health"
	focusSetupSlots     = "setup.slots"
	focusSetupWidth     = "setup.width"
	focusSetupHeight    = "setup.height"
	focusSetupRules     = "setup.rules"
	focusSetupSeed      = "setup.seed"
	focusSetupStart     = "setup.start"
	focusPause          = "play.pause"
	focusAbort          = "play.abort"
	focusGrid           = "play.grid"
	focusFlash          = "play.flash"
	focusMotion         = "play.motion"
	focusPlanTeach      = "play.plan-teach"
	focusPlan           = "play.plan"
	focusInspectID      = "inspect.id"
	focusInspectVersion = "inspect.version"
	focusInspectFilter  = "inspect.filter"
	focusInspectGo      = "inspect.go"
	focusInspectPrev    = "inspect.prev"
	focusInspectNext    = "inspect.next"
	focusSharePolicy    = "share.policy"
	focusShareRecipient = "share.recipient"
	focusShareSources   = "share.sources"
	focusShareSeed      = "share.seed"
	focusShareNoise     = "share.noise"
	focusShareRun       = "share.run"
)

type Shell struct {
	Model  *Model
	Client *HTTPClient
	theme  *material.Theme
	window *app.Window

	refresh, play, start widget.Clickable
	games, brains        widget.Clickable
	tournament, inspect  widget.Clickable
	experiments          widget.Clickable
	setupSlots           widget.Clickable
	setupWidth           widget.Clickable
	setupHeight          widget.Clickable
	setupRules           widget.Clickable
	setupSeed            widget.Clickable
	slotID               [4]widget.Clickable
	slotName             [4]widget.Clickable
	slotController       [4]widget.Clickable
	slotBrain            [4]widget.Clickable
	slotStartX           [4]widget.Clickable
	slotStartY           [4]widget.Clickable
	pause, abort         widget.Clickable
	grid, flash, motion  widget.Clickable
	planTeach, plan      widget.Clickable
	directions           [6]widget.Clickable
	inspectID            widget.Clickable
	inspectVersion       widget.Clickable
	inspectFilter        widget.Clickable
	inspectPrev          widget.Clickable
	inspectNext          widget.Clickable
	activeBrain          widget.Clickable
	sharePolicy          widget.Clickable
	shareRecipient       widget.Clickable
	shareSources         widget.Clickable
	shareSeed            widget.Clickable
	shareNoise           widget.Clickable
	shareRun             widget.Clickable

	gamesList, brainsList, inspectorList, tournamentList, setupList, shareList widget.List
	gameClicks, brainClicks, ruleClicks                                        []widget.Clickable

	keyTag, pointerTag                                          struct{}
	focused                                                     string
	editFresh                                                   bool
	keyboardFocused                                             bool
	pressedKeys                                                 map[key.Name]bool
	pointerDown                                                 map[pointer.ID]bool
	actionInFlight, pauseRequested, pauseTarget, abortRequested atomic.Bool
	scheduler                                                   TickScheduler
	selectedRule                                                int
	handledNavPress                                             [navButtonCount]time.Time
	handledNavClick                                             [navButtonCount]bool
}

func NewShell(baseURL string) *Shell {
	theme := material.NewTheme()
	// Embedded, verified Go fonts are deliberate: WASM never depends on a host
	// font or a cross-origin font request.
	theme.Shaper = text.NewShaper(text.NoSystemFonts(), text.WithCollection(gofont.Collection()))
	theme.Palette = material.Palette{Bg: design.Canvas, Fg: design.Text, ContrastBg: design.Accent, ContrastFg: design.AccentText}
	s := &Shell{Model: NewModel(), Client: NewHTTPClient(baseURL), theme: theme, pressedKeys: make(map[key.Name]bool), pointerDown: make(map[pointer.ID]bool), selectedRule: -1}
	s.gamesList.Axis = layout.Vertical
	s.brainsList.Axis = layout.Vertical
	s.inspectorList.Axis = layout.Vertical
	s.tournamentList.Axis = layout.Vertical
	s.setupList.Axis = layout.Vertical
	s.shareList.Axis = layout.Vertical
	return s
}

// Run starts a light-on-dark window and immediately probes the versioned API.
// The native binary and js/wasm binary use exactly the same event loop.
func Run() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("Worms"), app.Size(design.WindowWidth, design.WindowHeight))
		shell := NewShell("")
		shell.window = w
		installBrowserHooks(shell)
		go shell.probeHealth(w)
		if id := loadPersistedGame(); id != "" {
			go shell.resumeGame(id)
		}
		if err := shell.draw(w); err != nil {
			log.Printf("client window closed: %v", err)
		}
	}()
	app.Main()
}

func (s *Shell) probeHealth(w *app.Window) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r := <-s.Client.HealthAsync(ctx)
	if s.Client.IsCurrentFor(resourceHealth, r.Sequence) {
		s.Model.SetHealth(r.Value, r.Err)
	}
	w.Invalidate()
}

func (s *Shell) draw(w *app.Window) error {
	var ops op.Ops
	for {
		ev := w.Event()
		switch e := ev.(type) {
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			s.frame(gtx)
			e.Frame(gtx.Ops)
		case app.DestroyEvent:
			return e.Err
		}
	}
}

func (s *Shell) frame(gtx layout.Context) {
	paint.Fill(gtx.Ops, design.Canvas)
	event.Op(gtx.Ops, &s.keyTag)
	key.InputHintOp{Tag: &s.keyTag, Hint: key.HintText}.Add(gtx.Ops)
	if !s.keyboardFocused {
		gtx.Source.Execute(key.FocusCmd{Tag: &s.keyTag})
		s.keyboardFocused = true
	}
	filters := []event.Filter{
		key.FocusFilter{Target: &s.keyTag},
		key.Filter{Focus: &s.keyTag, Name: key.NameTab, Optional: key.ModShift},
		key.Filter{Focus: &s.keyTag, Name: key.NameEscape},
		key.Filter{Focus: &s.keyTag, Name: key.NameReturn},
		key.Filter{Focus: &s.keyTag, Name: key.NameEnter},
		key.Filter{Focus: &s.keyTag, Name: key.NameDeleteBackward},
		key.Filter{Focus: &s.keyTag, Name: ""},
	}
	for {
		ev, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}
		switch value := ev.(type) {
		case key.Event:
			s.key(value)
		case key.EditEvent:
			s.editText(value.Text)
		}
	}
	s.scheduleAutonomous(gtx)

	inset := layout.UniformInset(design.Space4)
	if gtx.Constraints.Max.X < gtx.Dp(design.CompactInsetWidth) {
		inset = layout.UniformInset(design.Space2)
	}
	inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{
			layout.Rigid(s.header),
			layout.Flexed(1, s.content),
		}
		if gtx.Constraints.Max.Y >= gtx.Dp(design.FooterMinHeight) {
			children = append(children, layout.Rigid(s.footer))
		}
		return layout.Flex{Axis: layout.Vertical, Spacing: layout.SpaceBetween}.Layout(gtx, children...)
	})
}

func (s *Shell) key(k key.Event) {
	if k.State == key.Release {
		delete(s.pressedKeys, k.Name)
		return
	}
	if k.State != key.Press || s.pressedKeys[k.Name] {
		return
	}
	s.pressedKeys[k.Name] = true
	v := s.Model.Snapshot()
	if k.Name == key.NameTab {
		s.moveFocus(k.Modifiers&key.ModShift != 0)
		return
	}
	if k.Name == key.NameDeleteBackward && s.isEditableFocus() {
		s.backspace()
		return
	}
	if k.Name == key.NameReturn || k.Name == key.NameEnter || (k.Name == key.NameSpace && !s.isEditableFocus()) {
		s.activateFocus()
		return
	}
	if k.Name == key.NameEscape && v.Screen == ScreenPlay {
		s.requestPause(!v.HUD.Paused)
		return
	}
	if v.Screen == ScreenPlay && (k.Name == "F" || k.Name == "f") {
		s.Model.ToggleFlash()
		s.requestFrame()
		return
	}
	if v.Screen == ScreenPlay && (k.Name == "G" || k.Name == "g") {
		s.Model.ToggleGrid()
		s.requestFrame()
		return
	}
	if s.isEditableFocus() {
		return
	}
	n := string(k.Name)
	if v.Screen == ScreenPlay && len(n) == 1 && n >= "1" && n <= "9" {
		s.Model.SetSpeed(int(n[0] - '0'))
		s.scheduler.Reset()
		s.requestFrame()
		return
	}
	if d, ok := DirectionFromKey(string(k.Name)); ok && v.Screen == ScreenPlay {
		go s.submitDirection(d)
	}
}

func (s *Shell) editText(value string) {
	if !s.isEditableFocus() || value == "" || value == "\n" || value == "\t" {
		return
	}
	v := s.Model.Snapshot()
	if s.editFresh {
		s.clearFocusedText(&v)
		s.editFresh = false
	}
	s.appendFocusedText(&v, value)
	s.applyEditedView(v)
}

func (s *Shell) backspace() {
	v := s.Model.Snapshot()
	s.editFresh = false
	trim := func(value string) string {
		runes := []rune(value)
		if len(runes) == 0 {
			return ""
		}
		return string(runes[:len(runes)-1])
	}
	s.transformFocusedText(&v, trim)
	s.applyEditedView(v)
}

func (s *Shell) clearFocusedText(v *AppView) {
	s.transformFocusedText(v, func(string) string { return "" })
}
func (s *Shell) appendFocusedText(v *AppView, text string) {
	s.transformFocusedText(v, func(value string) string { return value + text })
}

func (s *Shell) transformFocusedText(v *AppView, transform func(string) string) {
	switch s.focused {
	case focusSetupSeed:
		v.Setup.Seed = transform(v.Setup.Seed)
	case focusSetupWidth:
		v.Setup.Width = parsedInt(transform(strconv.Itoa(v.Setup.Width)))
		v.Setup.fitStarts()
	case focusSetupHeight:
		v.Setup.Height = parsedInt(transform(strconv.Itoa(v.Setup.Height)))
		v.Setup.fitStarts()
	case focusInspectID:
		v.Inspect.BrainID = transform(v.Inspect.BrainID)
		v.Inspect.Offset = 0
	case focusInspectVersion:
		v.Inspect.Version = transform(v.Inspect.Version)
		v.Inspect.Offset = 0
	case focusInspectFilter:
		v.Inspect.Filter = transform(v.Inspect.Filter)
		v.Inspect.Offset = 0
	case focusShareRecipient:
		v.Share.RecipientVersionID = transform(v.Share.RecipientVersionID)
	case focusShareSources:
		v.Share.SourceVersionIDs = transform(v.Share.SourceVersionIDs)
	case focusShareSeed:
		v.Share.Seed = transform(v.Share.Seed)
	case focusShareNoise:
		v.Share.NoiseRate = transform(v.Share.NoiseRate)
	default:
		for i := range v.Setup.Slots {
			switch s.focused {
			case setupFocus(i, "id"):
				v.Setup.Slots[i].ID = transform(v.Setup.Slots[i].ID)
			case setupFocus(i, "name"):
				v.Setup.Slots[i].Name = transform(v.Setup.Slots[i].Name)
			case setupFocus(i, "brain"):
				v.Setup.Slots[i].BrainID = transform(v.Setup.Slots[i].BrainID)
			case setupFocus(i, "x"):
				v.Setup.Slots[i].Start.X = parsedInt(transform(strconv.Itoa(v.Setup.Slots[i].Start.X)))
			case setupFocus(i, "y"):
				v.Setup.Slots[i].Start.Y = parsedInt(transform(strconv.Itoa(v.Setup.Slots[i].Start.Y)))
			}
		}
	}
}

func parsedInt(value string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(value))
	return n
}
func (s *Shell) applyEditedView(v AppView) {
	s.Model.SetSetup(v.Setup)
	s.Model.SetInspectorQuery(v.Inspect)
	s.Model.SetShare(v.Share)
	s.requestFrame()
}

func (s *Shell) isEditableFocus() bool {
	switch s.focused {
	case focusSetupSeed, focusSetupWidth, focusSetupHeight,
		focusInspectID, focusInspectVersion, focusInspectFilter,
		focusShareRecipient, focusShareSources, focusShareSeed, focusShareNoise:
		return true
	}
	return strings.HasPrefix(s.focused, "setup.slot.") && !strings.HasSuffix(s.focused, ".controller")
}

func setupFocus(slot int, field string) string { return fmt.Sprintf("setup.slot.%d.%s", slot, field) }
func directionFocus(direction int) string      { return fmt.Sprintf("play.direction.%d", direction) }
func gameFocus(index int) string               { return fmt.Sprintf("games.%d", index) }
func brainFocus(index int) string              { return fmt.Sprintf("brains.%d", index) }
func ruleFocus(index int) string               { return fmt.Sprintf("inspect.rule.%d", index) }

func (s *Shell) focusOrder() []string {
	v := s.Model.Snapshot()
	order := []string{focusNavPlay, focusNavGames, focusNavBrains, focusNavTournament, focusNavExperiments, focusNavHealth}
	switch v.Screen {
	case ScreenSetup:
		order = append(order, focusSetupSlots, focusSetupWidth, focusSetupHeight, focusSetupRules, focusSetupSeed)
		for i := range min(v.Setup.SlotCount, len(v.Setup.Slots)) {
			order = append(order, setupFocus(i, "id"), setupFocus(i, "name"), setupFocus(i, "controller"), setupFocus(i, "brain"), setupFocus(i, "x"), setupFocus(i, "y"))
		}
		order = append(order, focusSetupStart)
	case ScreenPlay:
		order = append(order, focusPause, focusAbort, focusGrid, focusFlash, focusMotion)
		if v.Board.Pending != nil {
			order = append(order, focusPlanTeach, focusPlan)
		}
		for i := range 6 {
			order = append(order, directionFocus(i))
		}
	case ScreenGames:
		for i := range len(v.Games) {
			order = append(order, gameFocus(i))
		}
	case ScreenBrains:
		for i := range len(v.Brains) {
			order = append(order, brainFocus(i))
		}
		order = append(order, focusInspectID, focusInspectGo)
	case ScreenInspector:
		order = append(order, focusInspectID, focusInspectVersion, focusInspectFilter, focusInspectGo, focusInspectPrev, focusInspectNext)
		for i := range len(inspectorRules(v.Inspector)) {
			order = append(order, ruleFocus(i))
		}
	case ScreenExperiments:
		order = append(order, focusSharePolicy, focusShareRecipient, focusShareSources, focusShareSeed, focusShareNoise, focusShareRun)
	case ScreenError:
		order = append(order, focusNavHealth)
	}
	return order
}

func (s *Shell) moveFocus(backward bool) {
	order := s.focusOrder()
	if len(order) == 0 {
		return
	}
	index := -1
	for i, value := range order {
		if value == s.focused {
			index = i
			break
		}
	}
	if backward {
		index--
		if index < 0 {
			index = len(order) - 1
		}
	} else {
		index = (index + 1) % len(order)
	}
	s.focused, s.editFresh = order[index], true
	s.requestFrame()
}

func (s *Shell) activateFocus() {
	v := s.Model.Snapshot()
	switch s.focused {
	case focusNavPlay:
		if v.Screen == ScreenSetup || v.GameID == "" {
			go s.startGame()
		} else {
			s.Model.Navigate(ScreenPlay)
			go s.resumeGame(v.GameID)
		}
	case focusNavGames:
		s.Model.Navigate(ScreenGames)
		go s.loadGames()
	case focusNavBrains:
		s.Model.Navigate(ScreenBrains)
		go s.loadBrains()
	case focusNavTournament:
		s.Model.Navigate(ScreenTournament)
		go s.loadTournament()
	case focusNavExperiments:
		s.Model.Navigate(ScreenExperiments)
	case focusNavHealth:
		go s.retry(v.Error.Retry)
	case focusSetupSlots:
		v.Setup.SlotCount = v.Setup.SlotCount%4 + 1
		s.Model.SetSetup(v.Setup)
	case focusSetupRules:
		v.Setup.Ruleset = nextRuleset(v.Setup.Ruleset)
		s.Model.SetSetup(v.Setup)
	case focusSetupStart:
		go s.startGame()
	case focusPause:
		s.requestPause(!v.HUD.Paused)
	case focusAbort:
		s.requestAbort()
	case focusGrid:
		s.Model.ToggleGrid()
	case focusFlash:
		s.Model.ToggleFlash()
	case focusMotion:
		s.Model.ToggleReducedMotion()
	case focusPlanTeach:
		s.Model.SetPlannerTeach(!v.Planner.Teach)
	case focusPlan:
		if v.Board.Pending != nil {
			go s.planPending()
		}
	case focusSharePolicy:
		v.Share.Policy = nextSharingPolicy(v.Share.Policy)
		s.Model.SetShare(v.Share)
	case focusShareRun:
		go s.runShareExperiment()
	case focusInspectGo:
		go s.loadInspector()
	case focusInspectPrev:
		if v.Inspect.Offset > 0 {
			v.Inspect.Offset = max(0, v.Inspect.Offset-v.Inspect.Limit)
			s.Model.SetInspectorQuery(v.Inspect)
			go s.loadInspector()
		}
	case focusInspectNext:
		if inspectorHasNext(v) {
			v.Inspect.Offset += v.Inspect.Limit
			s.Model.SetInspectorQuery(v.Inspect)
			go s.loadInspector()
		}
	default:
		if strings.HasPrefix(s.focused, "setup.slot.") && strings.HasSuffix(s.focused, ".controller") {
			for i := range v.Setup.Slots {
				if s.focused == setupFocus(i, "controller") {
					v.Setup.Slots[i].Controller = nextController(v.Setup.Slots[i].Controller)
					s.Model.SetSetup(v.Setup)
					break
				}
			}
		} else if strings.HasPrefix(s.focused, "play.direction.") {
			index, _ := strconv.Atoi(strings.TrimPrefix(s.focused, "play.direction."))
			if index >= 0 && index < 6 {
				go s.submitDirection(Direction(index))
			}
		} else if strings.HasPrefix(s.focused, "games.") {
			index, _ := strconv.Atoi(strings.TrimPrefix(s.focused, "games."))
			if index >= 0 && index < len(v.Games) {
				go s.resumeGame(v.Games[index].ID)
			}
		} else if strings.HasPrefix(s.focused, "brains.") {
			index, _ := strconv.Atoi(strings.TrimPrefix(s.focused, "brains."))
			if index >= 0 && index < len(v.Brains) {
				v.Inspect.BrainID = v.Brains[index].ID
				v.Inspect.Offset = 0
				s.Model.SetInspectorQuery(v.Inspect)
				go s.loadInspector()
			}
		} else if strings.HasPrefix(s.focused, "inspect.rule.") {
			s.selectedRule, _ = strconv.Atoi(strings.TrimPrefix(s.focused, "inspect.rule."))
		}
	}
	s.requestFrame()
}

func nextController(current string) string {
	for i, kind := range controllerKinds {
		if current == kind {
			return controllerKinds[(i+1)%len(controllerKinds)]
		}
	}
	return ControllerNew
}

// clickedBeforeLayout drains pointer state transitions that a material button
// would otherwise consume while laying itself out. A browser can queue press
// and release before Gio draws the next frame, so keep polling while pressed
// or hover state changes.
func clickedBeforeLayout(gtx layout.Context, button *widget.Clickable) bool {
	clicked := false
	for {
		pressed, hovered := button.Pressed(), button.Hovered()
		if button.Clicked(gtx) {
			clicked = true
			continue
		}
		if pressed == button.Pressed() && hovered == button.Hovered() {
			return clicked
		}
	}
}

func (s *Shell) handleNavClicks(gtx layout.Context) {
	v := s.Model.Snapshot()
	if clickedBeforeLayout(gtx, &s.refresh) && s.acceptNavClick(navRefresh, &s.refresh) {
		s.focused = focusNavHealth
		go s.retry(v.Error.Retry)
	}
	if clickedBeforeLayout(gtx, &s.play) && s.acceptNavClick(navPlay, &s.play) {
		s.focused = focusNavPlay
		if v.Screen == ScreenSetup || v.GameID == "" {
			go s.startGame()
		} else {
			s.Model.Navigate(ScreenPlay)
			go s.resumeGame(v.GameID)
		}
	}
	if clickedBeforeLayout(gtx, &s.games) && s.acceptNavClick(navGames, &s.games) {
		s.focused = focusNavGames
		s.Model.Navigate(ScreenGames)
		go s.loadGames()
	}
	if clickedBeforeLayout(gtx, &s.brains) && s.acceptNavClick(navBrains, &s.brains) {
		s.focused = focusNavBrains
		s.Model.Navigate(ScreenBrains)
		go s.loadBrains()
	}
	if clickedBeforeLayout(gtx, &s.tournament) && s.acceptNavClick(navTournament, &s.tournament) {
		s.focused = focusNavTournament
		s.Model.Navigate(ScreenTournament)
		go s.loadTournament()
	}
	if clickedBeforeLayout(gtx, &s.experiments) && s.acceptNavClick(navExperiments, &s.experiments) {
		s.focused = focusNavExperiments
		s.Model.Navigate(ScreenExperiments)
	}
}

func (s *Shell) acceptNavClick(index int, button *widget.Clickable) bool {
	if s.consumeNavPress(index, button) {
		return true
	}
	if s.handledNavClick[index] {
		s.handledNavClick[index] = false
		return false
	}
	return true
}
func (s *Shell) consumeNavPress(index int, button *widget.Clickable) bool {
	history := button.History()
	for i := len(history) - 1; i >= 0; i-- {
		press := history[i]
		if press.Cancelled {
			continue
		}
		if !press.Start.After(s.handledNavPress[index]) {
			return false
		}
		s.handledNavPress[index] = press.Start
		return true
	}
	return false
}
func (s *Shell) replayLayoutClicks(gtx layout.Context) {
	buttons := [...]struct {
		index  int
		button *widget.Clickable
	}{{navRefresh, &s.refresh}, {navPlay, &s.play}, {navGames, &s.games}, {navBrains, &s.brains}, {navTournament, &s.tournament}, {navExperiments, &s.experiments}}
	replayed := false
	for _, item := range buttons {
		if s.consumeNavPress(item.index, item.button) {
			item.button.Click()
			s.handledNavClick[item.index] = true
			replayed = true
		}
	}
	if replayed {
		s.handleNavClicks(gtx)
	}
}

func (s *Shell) header(gtx layout.Context) layout.Dimensions {
	v := s.Model.Snapshot()
	status := "API offline"
	statusColor := design.Danger
	if v.HealthOK {
		status = fmt.Sprintf("API ok · SQLite %s · %d checks", v.Health.Database, v.Health.RecordedChecks)
		statusColor = design.Success
	}
	statusLabel := material.Label(s.theme, design.TypeMeta, status)
	statusLabel.Color = statusColor
	if gtx.Constraints.Max.X < gtx.Dp(design.HeaderCompactWidth) {
		return layout.Inset{Bottom: design.Space2}.Layout(gtx, statusLabel.Layout)
	}
	return layout.Inset{Bottom: design.Space3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Spacing: layout.SpaceBetween}.Layout(gtx,
			layout.Rigid(material.H4(s.theme, "WORMS // six-neighbor arena").Layout),
			layout.Rigid(statusLabel.Layout),
		)
	})
}

func (s *Shell) footer(gtx layout.Context) layout.Dimensions {
	v := s.Model.Snapshot()
	label := fmt.Sprintf("speed %d · %s · F persistent capture · G grid · ESC pause", v.HUD.Speed, map[bool]string{true: "paused", false: "running"}[v.HUD.Paused])
	style := material.Label(s.theme, design.TypeSmall, label)
	style.Color = design.TextMuted
	return layout.Inset{Top: design.Space2}.Layout(gtx, style.Layout)
}

func (s *Shell) content(gtx layout.Context) layout.Dimensions {
	v := s.Model.Snapshot()
	if gtx.Constraints.Max.X < gtx.Dp(design.NavigationStackWidth) {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(s.navHorizontal),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.screen(gtx, v) }),
		)
	}
	return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceEnd}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.screen(gtx, v) }),
		layout.Rigid(s.nav),
	)
}

func (s *Shell) nav(gtx layout.Context) layout.Dimensions { return s.navAxis(gtx, layout.Vertical) }
func (s *Shell) navHorizontal(gtx layout.Context) layout.Dimensions {
	return s.navAxis(gtx, layout.Horizontal)
}
func (s *Shell) navAxis(gtx layout.Context, axis layout.Axis) layout.Dimensions {
	s.handleNavClicks(gtx)
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.button(gtx, &s.play, "play", focusNavPlay, true) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, &s.games, "games", focusNavGames, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, &s.brains, "brains", focusNavBrains, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, &s.tournament, "tournament", focusNavTournament, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, &s.experiments, "experiments", focusNavExperiments, true)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, &s.refresh, "health", focusNavHealth, true)
		}),
	}
	dims := layout.Inset{Left: design.Space2, Bottom: design.Space2}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if axis == layout.Horizontal {
			return responsiveStrip(gtx, children)
		}
		return layout.Flex{Axis: axis, Spacing: layout.SpaceEvenly}.Layout(gtx, children...)
	})
	s.replayLayoutClicks(gtx)
	return dims
}

func responsiveStrip(gtx layout.Context, children []layout.FlexChild) layout.Dimensions {
	perRow := len(children)
	if gtx.Constraints.Max.X < gtx.Dp(design.StripTwoColumnWidth) {
		perRow = 2
	}
	if gtx.Constraints.Max.X < gtx.Dp(design.StripOneColumnWidth) {
		perRow = 1
	}
	if perRow >= len(children) {
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
	}
	rows := make([]layout.FlexChild, 0, (len(children)+perRow-1)/perRow)
	for start := 0; start < len(children); start += perRow {
		end := min(start+perRow, len(children))
		row := children[start:end]
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, row...)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
}

func (s *Shell) button(gtx layout.Context, click *widget.Clickable, label, focus string, enabled bool) layout.Dimensions {
	if !enabled {
		gtx = gtx.Disabled()
	}
	style := material.Button(s.theme, click, label)
	style.CornerRadius = design.RadiusSmall
	style.Background = design.SurfaceRaised
	style.Color = design.Text
	if !enabled {
		style.Background = design.Surface
		style.Color = design.Disabled
	}
	border := widget.Border{Color: design.SurfaceRaised, CornerRadius: design.RadiusSmall, Width: design.Border}
	if s.focused == focus {
		border.Color, border.Width = design.Focus, design.FocusBorder
	}
	return layout.Inset{Right: design.Space1, Bottom: design.Space1}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return border.Layout(gtx, style.Layout)
	})
}

func (s *Shell) field(gtx layout.Context, click *widget.Clickable, label, value, focus, errText string) layout.Dimensions {
	body := label + ": " + value
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.button(gtx, click, body, focus, true) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if errText == "" {
				return layout.Dimensions{}
			}
			style := material.Label(s.theme, design.TypeSmall, errText)
			style.Color = design.Danger
			return style.Layout(gtx)
		}),
	)
}

func (s *Shell) screen(gtx layout.Context, v AppView) layout.Dimensions {
	switch v.Screen {
	case ScreenPlay:
		return s.playScreen(gtx, v)
	case ScreenGames:
		return s.gamesScreen(gtx, v)
	case ScreenBrains:
		return s.brainScreen(gtx, v)
	case ScreenInspector:
		return s.inspectScreen(gtx, v)
	case ScreenTournament:
		return s.tournamentScreen(gtx, v)
	case ScreenExperiments:
		return s.experimentScreen(gtx, v)
	case ScreenError:
		return s.errorScreen(gtx, v)
	default:
		return s.setupScreen(gtx, v)
	}
}

func (s *Shell) processFieldClick(gtx layout.Context, click *widget.Clickable, focus string) {
	if click.Clicked(gtx) {
		s.focused, s.editFresh = focus, true
	}
}

func (s *Shell) setupScreen(gtx layout.Context, v AppView) layout.Dimensions {
	if s.setupSlots.Clicked(gtx) {
		s.focused = focusSetupSlots
		v.Setup.SlotCount = v.Setup.SlotCount%4 + 1
		s.Model.SetSetup(v.Setup)
	}
	s.processFieldClick(gtx, &s.setupWidth, focusSetupWidth)
	s.processFieldClick(gtx, &s.setupHeight, focusSetupHeight)
	if s.setupRules.Clicked(gtx) {
		s.focused = focusSetupRules
		v.Setup.Ruleset = nextRuleset(v.Setup.Ruleset)
		s.Model.SetSetup(v.Setup)
	}
	s.processFieldClick(gtx, &s.setupSeed, focusSetupSeed)
	for i := range min(v.Setup.SlotCount, len(v.Setup.Slots)) {
		s.processFieldClick(gtx, &s.slotID[i], setupFocus(i, "id"))
		s.processFieldClick(gtx, &s.slotName[i], setupFocus(i, "name"))
		if s.slotController[i].Clicked(gtx) {
			s.focused = setupFocus(i, "controller")
			v.Setup.Slots[i].Controller = nextController(v.Setup.Slots[i].Controller)
			s.Model.SetSetup(v.Setup)
		}
		s.processFieldClick(gtx, &s.slotBrain[i], setupFocus(i, "brain"))
		s.processFieldClick(gtx, &s.slotStartX[i], setupFocus(i, "x"))
		s.processFieldClick(gtx, &s.slotStartY[i], setupFocus(i, "y"))
	}
	if s.start.Clicked(gtx) {
		s.focused = focusSetupStart
		go s.startGame()
	}
	v = s.Model.Snapshot()
	heading := "Configure a classic arena"
	guidance := "One to four stable slots; asleep slots remain visible but do not act."
	if v.Setup.Ruleset == "modern" {
		heading = "Configure a bounded arena"
	} else if strings.HasPrefix(v.Setup.Ruleset, "variants-") {
		heading = "Configure an extension arena"
		guidance = "Preset rules are explicit and opt-in; classic games keep their original wire path."
	}
	items := 3 + v.Setup.SlotCount
	return s.setupList.Layout(gtx, items, func(gtx layout.Context, index int) layout.Dimensions {
		return layout.Inset{Right: design.Space3, Bottom: design.Space3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			switch index {
			case 0:
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(material.H3(s.theme, heading).Layout),
					layout.Rigid(material.Body1(s.theme, guidance).Layout),
				)
			case 1:
				return responsiveStrip(gtx, []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.button(gtx, &s.setupSlots, fmt.Sprintf("slots: %d", v.Setup.SlotCount), focusSetupSlots, true)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.field(gtx, &s.setupWidth, "width", strconv.Itoa(v.Setup.Width), focusSetupWidth, v.Setup.Errors["width"])
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.field(gtx, &s.setupHeight, "height", strconv.Itoa(v.Setup.Height), focusSetupHeight, v.Setup.Errors["height"])
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.button(gtx, &s.setupRules, "rules: "+v.Setup.Ruleset, focusSetupRules, true)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.field(gtx, &s.setupSeed, "seed", v.Setup.Seed, focusSetupSeed, v.Setup.Errors["seed"])
					}),
				})
			case items - 1:
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if len(v.Setup.Errors) == 0 {
							return layout.Dimensions{}
						}
						style := material.Body2(s.theme, "Correct the marked fields; every other selection is retained.")
						style.Color = design.Danger
						return style.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.button(gtx, &s.start, "create authoritative game", focusSetupStart, true)
					}),
				)
			default:
				i := index - 2
				slot := v.Setup.Slots[i]
				prefix := fmt.Sprintf("slot.%d.", i)
				return widget.Border{Color: design.SurfaceRaised, CornerRadius: design.RadiusMedium, Width: design.Border}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(design.Space2).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(material.H6(s.theme, fmt.Sprintf("Slot %d · %s", i+1, map[bool]string{true: "ASLEEP", false: "ACTIVE"}[slot.Controller == ControllerAsleep])).Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return s.field(gtx, &s.slotID[i], "stable ID", slot.ID, setupFocus(i, "id"), v.Setup.Errors[prefix+"id"])
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return s.field(gtx, &s.slotName[i], "name", slot.Name, setupFocus(i, "name"), "")
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return s.button(gtx, &s.slotController[i], "controller: "+slot.Controller, setupFocus(i, "controller"), true)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return s.field(gtx, &s.slotBrain[i], "brain version ID", slot.BrainID, setupFocus(i, "brain"), v.Setup.Errors[prefix+"brain"])
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return s.field(gtx, &s.slotStartX[i], "start column", strconv.Itoa(slot.Start.X), setupFocus(i, "x"), v.Setup.Errors[prefix+"start"])
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										return s.field(gtx, &s.slotStartY[i], "start row", strconv.Itoa(slot.Start.Y), setupFocus(i, "y"), "")
									}),
								)
							}),
						)
					})
				})
			}
		})
	})
}

func (s *Shell) gamesScreen(gtx layout.Context, v AppView) layout.Dimensions {
	s.ensureClicks(&s.gameClicks, len(v.Games))
	for i := range v.Games {
		if s.gameClicks[i].Clicked(gtx) {
			s.focused = gameFocus(i)
			go s.resumeGame(v.Games[i].ID)
		}
	}
	if len(v.Games) == 0 {
		return s.emptyState(gtx, "No saved games", "Create a configured arena; its committed ID will appear here.")
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H3(s.theme, "Saved games").Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return s.gamesList.Layout(gtx, len(v.Games), func(gtx layout.Context, i int) layout.Dimensions {
				g := v.Games[i]
				return s.button(gtx, &s.gameClicks[i], fmt.Sprintf("%s · %s · seed %d · cursor %d", g.ID, g.Status, g.Seed, max(g.Cursor, g.Sequence)), gameFocus(i), true)
			})
		}),
	)
}

func (s *Shell) brainScreen(gtx layout.Context, v AppView) layout.Dimensions {
	s.ensureClicks(&s.brainClicks, len(v.Brains))
	for i := range v.Brains {
		if s.brainClicks[i].Clicked(gtx) {
			s.focused = brainFocus(i)
			v.Inspect.BrainID, v.Inspect.Offset = v.Brains[i].ID, 0
			s.Model.SetInspectorQuery(v.Inspect)
			go s.loadInspector()
		}
	}
	s.processFieldClick(gtx, &s.inspectID, focusInspectID)
	if s.inspect.Clicked(gtx) {
		s.focused = focusInspectGo
		go s.loadInspector()
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H3(s.theme, "Brain directory").Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return responsiveStrip(gtx, []layout.FlexChild{
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.field(gtx, &s.inspectID, "stable brain ID", v.Inspect.BrainID, focusInspectID, v.Inspect.Error)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.inspect, "inspect", focusInspectGo, strings.TrimSpace(v.Inspect.BrainID) != "")
				}),
			})
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if len(v.Brains) == 0 {
				return s.emptyState(gtx, "No brains stored", "Paste any stable brain ID above to inspect it or diagnose it.")
			}
			return s.brainsList.Layout(gtx, len(v.Brains), func(gtx layout.Context, i int) layout.Dimensions {
				brain := v.Brains[i]
				return s.button(gtx, &s.brainClicks[i], fmt.Sprintf("%s · %s · %s", brain.Name, brain.ID, brain.Description), brainFocus(i), true)
			})
		}),
	)
}

func inspectorRules(result InspectorResult) []InspectorRule {
	return result.Rules
}
func inspectorHasNext(v AppView) bool {
	if v.Inspector.NextOffset > v.Inspect.Offset {
		return true
	}
	return v.Inspector.Total > v.Inspect.Offset+max(1, v.Inspect.Limit)
}

func (s *Shell) inspectScreen(gtx layout.Context, v AppView) layout.Dimensions {
	s.processFieldClick(gtx, &s.inspectID, focusInspectID)
	s.processFieldClick(gtx, &s.inspectVersion, focusInspectVersion)
	s.processFieldClick(gtx, &s.inspectFilter, focusInspectFilter)
	if s.inspect.Clicked(gtx) {
		s.focused = focusInspectGo
		go s.loadInspector()
	}
	if s.inspectPrev.Clicked(gtx) && v.Inspect.Offset > 0 {
		v.Inspect.Offset = max(0, v.Inspect.Offset-v.Inspect.Limit)
		s.Model.SetInspectorQuery(v.Inspect)
		go s.loadInspector()
	}
	if s.inspectNext.Clicked(gtx) && inspectorHasNext(v) {
		v.Inspect.Offset += v.Inspect.Limit
		s.Model.SetInspectorQuery(v.Inspect)
		go s.loadInspector()
	}
	rules := inspectorRules(v.Inspector)
	s.ensureClicks(&s.ruleClicks, len(rules))
	for i := range rules {
		if s.ruleClicks[i].Clicked(gtx) {
			s.focused, s.selectedRule = ruleFocus(i), i
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H3(s.theme, "Brain inspector").Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return responsiveStrip(gtx, []layout.FlexChild{
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.field(gtx, &s.inspectID, "brain ID", v.Inspect.BrainID, focusInspectID, v.Inspect.Error)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.field(gtx, &s.inspectVersion, "version", v.Inspect.Version, focusInspectVersion, "")
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.field(gtx, &s.inspectFilter, "rule filter", v.Inspect.Filter, focusInspectFilter, "")
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.inspect, "load", focusInspectGo, strings.TrimSpace(v.Inspect.BrainID) != "")
				}),
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			first, last := 0, 0
			if len(rules) > 0 {
				first, last = v.Inspector.Offset+1, v.Inspector.Offset+len(rules)
			}
			meta := fmt.Sprintf("brain %s · version %d · rules %d–%d of %d", v.Inspector.BrainID, v.Inspector.Version, first, last, v.Inspector.Total)
			return material.Body2(s.theme, meta).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.inspectPrev, "previous page", focusInspectPrev, v.Inspect.Offset > 0)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.inspectNext, "next page", focusInspectNext, inspectorHasNext(v))
				}),
			)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if len(rules) == 0 {
				return s.emptyState(gtx, "No matching rules", "Change the version or filter; the selected brain remains active.")
			}
			return s.inspectorList.Layout(gtx, len(rules), func(gtx layout.Context, i int) layout.Dimensions {
				rule := rules[i]
				label := ruleDescription(rule)
				return s.button(gtx, &s.ruleClicks[i], label, ruleFocus(i), true)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if s.selectedRule < 0 || s.selectedRule >= len(rules) {
				return material.Body2(s.theme, "Select a rule to expose its six directional spokes.").Layout(gtx)
			}
			style := material.Body1(s.theme, "Selected · "+ruleDiagram(rules[s.selectedRule]))
			style.Color = design.Warning
			return style.Layout(gtx)
		}),
	)
}

func ruleDescription(rule InspectorRule) string {
	provenance := "provenance —"
	if len(rule.Provenance) > 0 {
		keys := make([]string, 0, len(rule.Provenance))
		for key := range rule.Provenance {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+rule.Provenance[key])
		}
		provenance = "provenance " + strings.Join(parts, ", ")
	}
	return fmt.Sprintf("mask %06b · orientation %s · action %s (%d) · %s", rule.Mask, directionName(rule.Orientation), rule.ActionName, rule.Action, provenance)
}
func ruleDiagram(rule InspectorRule) string {
	parts := make([]string, 6)
	for d, name := range directionNames {
		state := "clear"
		if rule.TrailMask&(1<<d) != 0 || rule.Mask&(1<<d) != 0 {
			state = "trail"
		}
		if rule.OccupiedMask&(1<<d) != 0 {
			state = "occupied"
		}
		if rule.Action == d {
			state += "+ACTION"
		}
		if rule.Incoming == d || rule.Orientation == d {
			state += "+INCOMING"
		}
		parts[d] = name + "[" + state + "]"
	}
	legal := make([]string, 0, len(rule.Legal))
	for _, d := range rule.Legal {
		legal = append(legal, directionName(d))
	}
	return strings.Join(parts, "  ") + " · legal " + strings.Join(legal, ",")
}
func directionName(direction int) string {
	if direction >= 0 && direction < len(directionNames) {
		return directionNames[direction]
	}
	return "none"
}

var sharingPolicies = [...]string{"none", "same_team", "all_worms", "seeded_noisy"}

func nextSharingPolicy(current string) string {
	for i, policy := range sharingPolicies {
		if policy == current {
			return sharingPolicies[(i+1)%len(sharingPolicies)]
		}
	}
	return sharingPolicies[0]
}

func variantHUDLabel(v AppView) string {
	parts := make([]string, 0, 3)
	if v.HUD.HasEnergy {
		parts = append(parts, fmt.Sprintf("ENERGY %d", v.HUD.Energy))
	}
	if v.HUD.Team != "" {
		parts = append(parts, fmt.Sprintf("TEAM %s · score %d", v.HUD.Team, v.HUD.TeamScore))
	}
	if v.Board.UnknownCount > 0 {
		parts = append(parts, fmt.Sprintf("FOG %d unknown cells", v.Board.UnknownCount))
	}
	return strings.Join(parts, " · ")
}

func scoreHUDLabel(score ScoreView) string {
	state := "alive"
	if !score.Alive {
		state = "DEAD"
	} else if score.Asleep {
		state = "ASLEEP"
	}
	active := ""
	if score.Active {
		active = " ACTIVE"
	}
	if score.Team == "" {
		return fmt.Sprintf("■ %s [%s%s] %d · %s", score.Name, state, active, score.Score, score.Controller)
	}
	return fmt.Sprintf("WORM ■ %s [%s%s] %d · %s · TEAM %s", score.Name, state, active, score.Score, score.Controller, score.Team)
}

func plannerAlternativeLabel(alternative PlannerAlternative) string {
	state := "candidate"
	if alternative.Chosen {
		state = "CHOSEN"
	}
	return fmt.Sprintf("%s %s · total %d · capture %d · border %d · survival %d · %s", state, directionName(alternative.Action), alternative.Total, alternative.Capture, alternative.Border, alternative.Survival, alternative.Reason)
}

func (s *Shell) tournamentScreen(gtx layout.Context, v AppView) layout.Dimensions {
	count := len(v.Tournaments) + len(v.Matches)
	if count == 0 {
		return s.emptyState(gtx, "No tournament records", "Completed and running matches will appear here with their game IDs.")
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H3(s.theme, "Tournament").Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return s.tournamentList.Layout(gtx, count, func(gtx layout.Context, i int) layout.Dimensions {
				if i < len(v.Tournaments) {
					t := v.Tournaments[i]
					return material.Body1(s.theme, fmt.Sprintf("%s · %s · %s", t.Name, t.ID, t.Status)).Layout(gtx)
				}
				m := v.Matches[i-len(v.Tournaments)]
				return material.Body2(s.theme, fmt.Sprintf("round %d · %s · game %s · %s", m.Round, m.ID, m.GameID, m.Status)).Layout(gtx)
			})
		}),
	)
}

func (s *Shell) experimentScreen(gtx layout.Context, v AppView) layout.Dimensions {
	if s.sharePolicy.Clicked(gtx) {
		s.focused = focusSharePolicy
		v.Share.Policy = nextSharingPolicy(v.Share.Policy)
		s.Model.SetShare(v.Share)
	}
	s.processFieldClick(gtx, &s.shareRecipient, focusShareRecipient)
	s.processFieldClick(gtx, &s.shareSources, focusShareSources)
	s.processFieldClick(gtx, &s.shareSeed, focusShareSeed)
	s.processFieldClick(gtx, &s.shareNoise, focusShareNoise)
	if s.shareRun.Clicked(gtx) && !v.Share.Running {
		s.focused = focusShareRun
		go s.runShareExperiment()
	}
	v = s.Model.Snapshot()
	return s.shareList.Layout(gtx, 4, func(gtx layout.Context, index int) layout.Dimensions {
		return layout.Inset{Right: design.Space3, Bottom: design.Space3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			switch index {
			case 0:
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(material.H3(s.theme, "Rule-sharing experiment").Layout),
					layout.Rigid(material.Body1(s.theme, "Derive an immutable brain version from explicit source versions; originals are never mutated.").Layout),
				)
			case 1:
				return responsiveStrip(gtx, []layout.FlexChild{
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.button(gtx, &s.sharePolicy, "policy: "+v.Share.Policy, focusSharePolicy, true)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.field(gtx, &s.shareRecipient, "recipient version ID", v.Share.RecipientVersionID, focusShareRecipient, "")
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.field(gtx, &s.shareSources, "source version IDs (comma separated)", v.Share.SourceVersionIDs, focusShareSources, "")
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.field(gtx, &s.shareSeed, "seed", v.Share.Seed, focusShareSeed, "")
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.field(gtx, &s.shareNoise, "noise rate 0–1", v.Share.NoiseRate, focusShareNoise, "")
					}),
				})
			case 2:
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "run sharing experiment"
						if v.Share.Running {
							label = "experiment running…"
						}
						return s.button(gtx, &s.shareRun, label, focusShareRun, !v.Share.Running)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if v.Share.Error == "" {
							return layout.Dimensions{}
						}
						style := material.Body2(s.theme, "Experiment error: "+v.Share.Error)
						style.Color = design.Danger
						return style.Layout(gtx)
					}),
				)
			default:
				if v.Share.Result == nil {
					return s.emptyState(gtx, "No experiment result yet", "Choose a recipient and at least one source version, then run the experiment.")
				}
				result := v.Share.Result
				versionLabel := fmt.Sprintf("%d immutable versions persisted", len(result.BrainVersions))
				if len(result.BrainVersions) > 0 {
					version := result.BrainVersions[0]
					versionLabel = fmt.Sprintf("%s · brain %s · version %d", version.ID, version.BrainID, version.Version)
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(material.H5(s.theme, "Derived immutable version").Layout),
					layout.Rigid(material.Body1(s.theme, versionLabel).Layout),
					layout.Rigid(material.Body2(s.theme, fmt.Sprintf("%s · %d derived · %d versions · %d changes · +%d / −%d", result.Policy, result.Metrics.Derived, result.Metrics.Versions, result.Metrics.Changes, result.Metrics.Additions, result.Metrics.Removals)).Layout),
					layout.Rigid(material.Body2(s.theme, "result hash "+result.Hash).Layout),
				)
			}
		})
	})
}

func (s *Shell) errorScreen(gtx layout.Context, v AppView) layout.Dimensions {
	code := v.Error.Code
	if code == "" {
		code = "request_failed"
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H3(s.theme, "The request was not committed").Layout),
		layout.Rigid(material.Body1(s.theme, code+" · "+v.Error.Message).Layout),
		layout.Rigid(material.Body2(s.theme, "Your last authoritative cursor and setup selections are retained. Retry cannot duplicate a committed transition.").Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, &s.refresh, "retry", focusNavHealth, true)
		}),
	)
}

func (s *Shell) emptyState(gtx layout.Context, title, guidance string) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H4(s.theme, title).Layout),
		layout.Rigid(material.Body1(s.theme, guidance).Layout),
	)
}

func (s *Shell) playScreen(gtx layout.Context, v AppView) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.hud(gtx, v) }),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.board(gtx, v) }),
	)
}

func (s *Shell) hud(gtx layout.Context, v AppView) layout.Dimensions {
	if s.pause.Clicked(gtx) {
		s.focused = focusPause
		s.requestPause(!v.HUD.Paused)
	}
	if s.abort.Clicked(gtx) {
		s.focused = focusAbort
		s.requestAbort()
	}
	if s.grid.Clicked(gtx) {
		s.focused = focusGrid
		s.Model.ToggleGrid()
	}
	if s.flash.Clicked(gtx) {
		s.focused = focusFlash
		s.Model.ToggleFlash()
	}
	if s.motion.Clicked(gtx) {
		s.focused = focusMotion
		s.Model.ToggleReducedMotion()
	}
	if s.planTeach.Clicked(gtx) {
		s.focused = focusPlanTeach
		s.Model.SetPlannerTeach(!v.Planner.Teach)
	}
	if s.plan.Clicked(gtx) && v.Board.Pending != nil {
		s.focused = focusPlan
		go s.planPending()
	}
	for i := range s.directions {
		if s.directions[i].Clicked(gtx) {
			s.focused = directionFocus(i)
			go s.submitDirection(Direction(i))
		}
	}
	if s.activeBrain.Clicked(gtx) {
		for _, score := range v.HUD.Scores {
			if score.Active && score.BrainID != "" {
				v.Inspect.BrainID, v.Inspect.Offset = score.BrainID, 0
				s.Model.SetInspectorQuery(v.Inspect)
				go s.loadInspector()
				break
			}
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return responsiveStrip(gtx, []layout.FlexChild{
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.pause, map[bool]string{true: "resume (ESC)", false: "pause (ESC)"}[v.HUD.Paused], focusPause, true)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.abort, "abort game", focusAbort, true)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.grid, map[bool]string{true: "grid G: on", false: "grid G: off"}[v.Toggles.Grid], focusGrid, true)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.flash, map[bool]string{true: "capture F: persistent", false: "capture F: once"}[v.Toggles.Flash], focusFlash, true)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, &s.motion, map[bool]string{true: "motion: reduced", false: "motion: standard"}[v.Toggles.ReducedMotion], focusMotion, true)
				}),
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Flexed(1, material.Body2(s.theme, v.HUD.Status).Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					for _, score := range v.HUD.Scores {
						if score.Active && score.BrainID != "" {
							return s.button(gtx, &s.activeBrain, "inspect active brain "+score.BrainID, "play.active-brain", true)
						}
					}
					return layout.Dimensions{}
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := variantHUDLabel(v)
			if label == "" {
				return layout.Dimensions{}
			}
			style := material.Label(s.theme, design.TypeMeta, label)
			style.Color = design.Warning
			return layout.Inset{Bottom: design.Space1}.Layout(gtx, style.Layout)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			children := make([]layout.FlexChild, 0, len(v.HUD.Scores))
			for _, score := range v.HUD.Scores {
				score := score
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					style := material.Label(s.theme, design.TypeMeta, scoreHUDLabel(score))
					style.Color = rgba(score.Color)
					return layout.Inset{Right: design.Space3}.Layout(gtx, style.Layout)
				}))
			}
			if v.Board.GameOver && len(v.HUD.Winners) > 0 {
				label := "WINNER: "
				if len(v.HUD.Winners) > 1 {
					label = "WINNERS: "
				}
				children = append(children, layout.Rigid(material.Body2(s.theme, label+strings.Join(v.HUD.Winners, " + ")).Layout))
			} else if v.Board.GameOver && v.HUD.Tie && len(v.HUD.Scores) > 0 {
				leaders := make([]string, 0, len(v.HUD.Scores))
				for _, score := range v.HUD.Scores {
					if score.Score == v.HUD.Scores[0].Score {
						leaders = append(leaders, score.Name)
					}
				}
				children = append(children, layout.Rigid(material.Body2(s.theme, "TIED WINNERS: "+strings.Join(leaders, " + ")).Layout))
			}
			if v.Board.GameOver && len(v.HUD.TeamWinners) > 0 {
				children = append(children, layout.Rigid(material.Body2(s.theme, "TEAM WINNERS: "+strings.Join(v.HUD.TeamWinners, " + ")).Layout))
			}
			return responsiveStrip(gtx, children)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			prompt := "Choose a legal direction"
			if v.Board.GameOver {
				prompt = "Game complete · legal moves closed"
			}
			if v.Board.Pending != nil {
				prompt = fmt.Sprintf("Teach %s · exact request %d · mask %06b", v.Board.Pending.WormID, v.Board.Pending.Request, v.Board.Pending.Mask)
			}
			sections := []layout.FlexChild{layout.Rigid(material.Body2(s.theme, prompt).Layout)}
			if v.Board.Pending != nil {
				sections = append(sections, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return responsiveStrip(gtx, []layout.FlexChild{
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							label := map[bool]string{true: "planner: teach chosen rule", false: "planner: preview only"}[v.Planner.Teach]
							return s.button(gtx, &s.planTeach, label, focusPlanTeach, true)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return s.button(gtx, &s.plan, "plan alternatives", focusPlan, !v.HUD.Paused)
						}),
					})
				}))
			}
			if v.Planner.Error != "" {
				style := material.Body2(s.theme, "Planner error: "+v.Planner.Error)
				style.Color = design.Danger
				sections = append(sections, layout.Rigid(style.Layout))
			}
			if v.Planner.Decision != nil {
				for _, alternative := range v.Planner.Decision.Alternatives {
					alternative := alternative
					sections = append(sections, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Label(s.theme, design.TypeSmall, plannerAlternativeLabel(alternative)).Layout(gtx)
					}))
				}
			}
			directions := make([]layout.FlexChild, 0, len(directionNames))
			for i, name := range directionNames {
				i, name := i, name
				directions = append(directions, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := name
					if !v.Board.Legal[i] {
						label += " · blocked"
					}
					return s.button(gtx, &s.directions[i], label, directionFocus(i), v.Board.Legal[i] && !v.HUD.Paused && !v.Board.GameOver)
				}))
			}
			sections = append(sections, layout.Rigid(func(gtx layout.Context) layout.Dimensions { return responsiveStrip(gtx, directions) }))
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, sections...)
		}),
	)
}

func (s *Shell) board(gtx layout.Context, v AppView) layout.Dimensions {
	size := gtx.Constraints.Max
	boardClip := clip.Rect{Max: size}.Push(gtx.Ops)
	defer boardClip.Pop()
	g := NewBoardGeometry(v.Board.Width, v.Board.Height, size)
	if g.Width < 1 || g.Height < 1 {
		g = NewBoardGeometry(18, 18, size)
	}
	hit := clip.Rect(g.ScreenBounds()).Push(gtx.Ops)
	event.Op(gtx.Ops, &s.pointerTag)
	hit.Pop()
	if v.Toggles.Grid {
		for y := range g.Height {
			for x := range g.Width {
				point := Point{X: x, Y: y}
				if v.Board.FogOfWar && !v.Board.Visible[point] {
					continue
				}
				FillTerritory(gtx.Ops, g, point, packed(design.Grid))
			}
		}
	}
	for point := range v.Board.Unknown {
		if !g.InBounds(point) {
			continue
		}
		FillTerritory(gtx.Ops, g, point, packed(design.Unknown))
		center := g.DotAt(point)
		radius := g.DotRadius * .3
		stroke(gtx.Ops, f32.Pt(center.X-radius, center.Y-radius), f32.Pt(center.X+radius, center.Y+radius), packed(design.TextMuted), float32(gtx.Dp(design.Border)))
		stroke(gtx.Ops, f32.Pt(center.X-radius, center.Y+radius), f32.Pt(center.X+radius, center.Y-radius), packed(design.TextMuted), float32(gtx.Dp(design.Border)))
	}
	for p, c := range v.Board.Territory {
		if !g.InBounds(p) {
			continue
		}
		FillTerritory(gtx.Ops, g, p, c)
		ownerCue(gtx.Ops, g, p, v.Board.TerritoryOwners[p])
	}
	for point := range v.Board.Obstacles {
		if !g.InBounds(point) || !v.Board.Visible[point] {
			continue
		}
		FillTerritory(gtx.Ops, g, point, packed(design.Obstacle))
		center := g.DotAt(point)
		for _, offset := range [...]float32{-.25, 0, .25} {
			stroke(gtx.Ops, f32.Pt(center.X-g.DotRadius*.45, center.Y+g.DotRadius*offset), f32.Pt(center.X+g.DotRadius*.45, center.Y+g.DotRadius*offset), packed(design.Canvas), float32(gtx.Dp(design.Border)))
		}
	}
	for point := range v.Board.Holes {
		if !g.InBounds(point) || !v.Board.Visible[point] {
			continue
		}
		FillTerritory(gtx.Ops, g, point, packed(design.Hole))
		center := g.DotAt(point)
		radius := g.DotRadius * .45
		stroke(gtx.Ops, f32.Pt(center.X-radius, center.Y-radius), f32.Pt(center.X+radius, center.Y+radius), packed(design.Danger), float32(gtx.Dp(design.FocusBorder)))
		stroke(gtx.Ops, f32.Pt(center.X-radius, center.Y+radius), f32.Pt(center.X+radius, center.Y-radius), packed(design.Danger), float32(gtx.Dp(design.FocusBorder)))
	}
	for point, weight := range v.Board.Weights {
		if !g.InBounds(point) || !v.Board.Visible[point] {
			continue
		}
		center := g.DotAt(point)
		count := min(weight, 3)
		for i := range count {
			offset := (float32(i) - float32(count-1)/2) * g.DotRadius * .25
			stroke(gtx.Ops, f32.Pt(center.X+offset, center.Y-g.DotRadius*.3), f32.Pt(center.X+offset, center.Y+g.DotRadius*.3), packed(design.Weight), float32(gtx.Dp(design.FocusBorder)))
		}
	}
	for _, trail := range v.Board.Trails {
		halves := EndpointHalfSegments(trail.A, trail.B, trail.AColor, trail.BColor, g)
		for _, seg := range halves {
			stroke(gtx.Ops, seg.From, seg.To, seg.Color, float32(gtx.Dp(design.Space1)))
		}
	}
	for _, worm := range v.Board.Worms {
		if !g.InBounds(worm.Position) {
			continue
		}
		p := g.DotAt(worm.Position)
		radius := g.DotRadius * .5
		if worm.Asleep {
			paint.FillShape(gtx.Ops, rgba(worm.Color), clip.Rect{Min: image.Pt(int(p.X-radius), int(p.Y-radius)), Max: image.Pt(int(p.X+radius), int(p.Y+radius))}.Op())
		} else {
			paint.FillShape(gtx.Ops, rgba(worm.Color), clip.Ellipse(image.Rect(int(p.X-radius), int(p.Y-radius), int(p.X+radius), int(p.Y+radius))).Op(gtx.Ops))
		}
		if !worm.Alive {
			stroke(gtx.Ops, f32.Pt(p.X-radius, p.Y-radius), f32.Pt(p.X+radius, p.Y+radius), packed(design.Danger), float32(gtx.Dp(design.FocusBorder)))
			stroke(gtx.Ops, f32.Pt(p.X-radius, p.Y+radius), f32.Pt(p.X+radius, p.Y-radius), packed(design.Danger), float32(gtx.Dp(design.FocusBorder)))
		}
		if worm.Active {
			stroke(gtx.Ops, f32.Pt(p.X-g.DotRadius*.8, p.Y), f32.Pt(p.X+g.DotRadius*.8, p.Y), packed(design.Focus), float32(gtx.Dp(design.FocusBorder)))
			stroke(gtx.Ops, f32.Pt(p.X, p.Y-g.DotRadius*.8), f32.Pt(p.X, p.Y+g.DotRadius*.8), packed(design.Focus), float32(gtx.Dp(design.FocusBorder)))
		}
	}
	s.renderCapture(gtx, g, v)
	for {
		ev, ok := gtx.Source.Event(pointer.Filter{Target: &s.pointerTag, Kinds: pointer.Press | pointer.Release | pointer.Cancel})
		if !ok {
			break
		}
		p, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		if p.Kind == pointer.Release || p.Kind == pointer.Cancel {
			delete(s.pointerDown, p.PointerID)
			continue
		}
		if s.pointerDown[p.PointerID] {
			continue
		}
		s.pointerDown[p.PointerID] = true
		if v.Screen != ScreenPlay || v.HUD.Paused || v.Board.GameOver || !g.ContainsScreen(p.Position) {
			continue
		}
		for _, worm := range v.Board.Worms {
			if !worm.Active || !g.InBounds(worm.Position) {
				continue
			}
			direction := DirectionFromPointer(g.DotAt(worm.Position), p.Position)
			if IsLegalDirection(v.Board.Legal, direction) {
				go s.submitDirection(direction)
			}
			break
		}
	}
	return layout.Dimensions{Size: size}
}

func ownerCue(ops *op.Ops, g BoardGeometry, p Point, owner string) {
	if owner == "" {
		return
	}
	center := g.DotAt(p)
	offset := float32((colorForID(owner, owner)>>8)%5) - 2
	stroke(ops, f32.Pt(center.X-g.DotRadius*.35, center.Y+offset), f32.Pt(center.X+g.DotRadius*.35, center.Y+offset), packed(design.Canvas), 1)
}

func (s *Shell) renderCapture(gtx layout.Context, g BoardGeometry, v AppView) {
	capture := v.Board.Capture
	if len(capture.Points) == 0 {
		return
	}
	visible := v.Toggles.Flash || gtx.Now.Before(capture.Until)
	if !visible {
		return
	}
	if !v.Toggles.Flash && gtx.Now.Before(capture.Until) {
		gtx.Execute(op.InvalidateCmd{At: minTime(capture.Until, gtx.Now.Add(design.CaptureFrame))})
	}
	for p, capturedColor := range capture.Points {
		if !g.InBounds(p) {
			continue
		}
		points := g.Territory(p)
		if v.Toggles.ReducedMotion {
			for i := range points {
				stroke(gtx.Ops, points[i], points[(i+1)%len(points)], capturedColor, float32(gtx.Dp(design.Space1)))
			}
			ownerCue(gtx.Ops, g, p, v.Board.TerritoryOwners[p])
			continue
		}
		phase := (gtx.Now.UnixMilli()/design.CaptureFrame.Milliseconds())%2 == 0
		if v.Toggles.Flash || phase {
			FillTerritory(gtx.Ops, g, p, capturedColor)
			for i := range points {
				stroke(gtx.Ops, points[i], points[(i+1)%len(points)], capturedColor, float32(gtx.Dp(design.Space1)))
			}
			ownerCue(gtx.Ops, g, p, v.Board.TerritoryOwners[p])
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func stroke(ops *op.Ops, a, b f32.Point, c uint32, width float32) {
	dx, dy := b.X-a.X, b.Y-a.Y
	length := float32(math.Hypot(float64(dx), float64(dy)))
	if length == 0 {
		return
	}
	nx, ny := -dy/length*width/2, dx/length*width/2
	var path clip.Path
	path.Begin(ops)
	path.MoveTo(f32.Pt(a.X+nx, a.Y+ny))
	path.LineTo(f32.Pt(b.X+nx, b.Y+ny))
	path.LineTo(f32.Pt(b.X-nx, b.Y-ny))
	path.LineTo(f32.Pt(a.X-nx, a.Y-ny))
	path.Close()
	paint.FillShape(ops, rgba(c), clip.Outline{Path: path.End()}.Op())
}
func rgba(c uint32) color.NRGBA {
	return color.NRGBA{R: byte(c >> 24), G: byte(c >> 16), B: byte(c >> 8), A: byte(c)}
}

func (s *Shell) ensureClicks(clicks *[]widget.Clickable, count int) {
	for len(*clicks) < count {
		*clicks = append(*clicks, widget.Clickable{})
	}
}
func (s *Shell) requestFrame() {
	if s.window != nil {
		s.window.Invalidate()
	}
}

func (s *Shell) loadGames() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	v, seq, err := s.Client.Games(ctx)
	if s.Client.IsCurrentFor(resourceGames, seq) {
		s.Model.SetGames(v, err)
	}
	s.requestFrame()
}
func (s *Shell) loadBrains() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	v, seq, err := s.Client.Brains(ctx)
	if s.Client.IsCurrentFor(resourceBrains, seq) {
		s.Model.SetBrains(v, err)
	}
	s.requestFrame()
}
func (s *Shell) loadInspector() {
	v := s.Model.Snapshot()
	id := strings.TrimSpace(v.Inspect.BrainID)
	if id == "" {
		s.Model.SetInspector(InspectorResult{}, errors.New("brain ID is required"))
		s.requestFrame()
		return
	}
	version := 0
	if strings.TrimSpace(v.Inspect.Version) != "" {
		var err error
		version, err = strconv.Atoi(v.Inspect.Version)
		if err != nil || version < 1 {
			s.Model.SetInspector(InspectorResult{}, errors.New("version must be a positive whole number"))
			s.requestFrame()
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, seq, err := s.Client.InspectPage(ctx, id, version, v.Inspect.Limit, strings.TrimSpace(v.Inspect.Filter), v.Inspect.Offset)
	if s.Client.IsCurrentFor(resourceInspector, seq) {
		s.Model.SetInspector(result, err)
	}
	s.requestFrame()
}
func (s *Shell) loadTournament() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tournaments, seq, err := s.Client.Tournaments(ctx)
	if !s.Client.IsCurrentFor(resourceTournament, seq) {
		return
	}
	s.Model.SetTournaments(tournaments, err)
	if err == nil && len(tournaments) > 0 {
		matches, matchSeq, matchErr := s.Client.Matches(ctx, tournaments[0].ID)
		if s.Client.IsCurrentFor(resourceTournament, matchSeq) {
			s.Model.SetMatches(matches, matchErr)
		}
	}
	s.requestFrame()
}
func (s *Shell) probeHealthFromModel() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r := <-s.Client.HealthAsync(ctx)
	if s.Client.IsCurrentFor(resourceHealth, r.Sequence) {
		s.Model.SetHealth(r.Value, r.Err)
	}
	s.requestFrame()
}
func (s *Shell) retry(kind string) {
	switch kind {
	case "retry games":
		s.loadGames()
	case "retry brains":
		s.loadBrains()
	case "retry inspector":
		s.loadInspector()
	case "retry tournament":
		s.loadTournament()
	case "retry game":
		s.resumeGame(s.Model.Snapshot().GameID)
	default:
		s.probeHealthFromModel()
	}
}

func payloadEnvelope(data any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"version": 1, "data": data})
	return raw
}
func (s *Shell) startGame() {
	setup, ok := s.Model.ValidateSetup()
	if !ok {
		s.requestFrame()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	id := fmt.Sprintf("ui-%d", time.Now().UnixNano())
	participants := make([]ParticipantRequest, 0, setup.SlotCount)
	for i := range setup.SlotCount {
		slot := setup.Slots[i]
		start := StatePoint{Q: slot.Start.X, R: slot.Start.Y}
		participants = append(participants, ParticipantRequest{ID: slot.ID, Name: slot.Name, Kind: slot.Controller, BrainVersionID: slot.BrainID, Color: slot.Color, Start: &start, Payload: payloadEnvelope(map[string]any{"start": start, "asleep": slot.Controller == ControllerAsleep})})
	}
	rules := payloadEnvelope(map[string]any{"ruleset": setup.Ruleset, "width": setup.Width, "height": setup.Height})
	game, _, err := s.Client.CreateGame(ctx, CreateGameRequest{ID: id, Status: "active", Ruleset: setup.Ruleset, Width: setup.Width, Height: setup.Height, RulesPayload: rules, Seed: setup.ResolvedSeed, Participants: participants, ExtensionConfig: setup.ExtensionPreset()})
	if err != nil {
		s.Model.SetGameError(fmt.Errorf("create game: %w", err))
		s.requestFrame()
		return
	}
	response, _, err := s.Client.Resume(ctx, game.ID)
	if err != nil {
		s.Model.SetGameError(fmt.Errorf("resume committed game %s: %w", game.ID, err))
		s.requestFrame()
		return
	}
	s.Model.SetGame(game.ID, response)
	persistGame(game.ID)
	s.scheduler.Reset()
	s.requestFrame()
}
func (s *Shell) resumeGame(id string) {
	if id == "" {
		s.Model.SetGameError(errors.New("no saved game ID is available to resume"))
		s.requestFrame()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	response, _, err := s.Client.Resume(ctx, id)
	if err != nil {
		s.Model.SetGameError(fmt.Errorf("resume game %s: %w", id, err))
		s.requestFrame()
		return
	}
	s.Model.SetGame(id, response)
	persistGame(id)
	s.scheduler.Reset()
	s.requestFrame()
}

func (s *Shell) planPending() {
	if !s.actionInFlight.CompareAndSwap(false, true) {
		return
	}
	defer s.finishAction()
	view := s.Model.Snapshot()
	if view.Board.Pending == nil || view.GameID == "" || view.HUD.Paused || view.Board.GameOver {
		return
	}
	config := view.Planner.Config
	if config.Seed == 0 {
		config.Seed = view.Setup.ResolvedSeed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, _, err := s.Client.Plan(ctx, view.GameID, PlanRequest{Cursor: view.GameCursor, EventHash: view.EventHash, WormID: view.Board.Pending.WormID, PlannerConfig: config, Teach: view.Planner.Teach})
	if err != nil {
		s.Model.SetPlannerResult(PlannerDecision{}, nil, fmt.Errorf("plan pending decision: %w", err))
		s.requestFrame()
		return
	}
	var applied *GameResponse
	if view.Planner.Teach && result.Game.ID != "" {
		response := result.GameResponse
		applied = &response
	}
	s.Model.SetPlannerResult(result.Decision, applied, nil)
	s.scheduler.Reset()
	s.requestFrame()
}

func (s *Shell) runShareExperiment() {
	view := s.Model.Snapshot()
	share := view.Share
	if share.Running {
		return
	}
	share.Running, share.Error, share.Result = true, "", nil
	s.Model.SetShare(share)
	s.requestFrame()
	recipient := strings.TrimSpace(share.RecipientVersionID)
	rawSources := strings.Split(share.SourceVersionIDs, ",")
	sources := make([]string, 0, len(rawSources))
	seen := make(map[string]bool, len(rawSources)+1)
	seen[recipient] = recipient != ""
	for _, value := range rawSources {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			sources = append(sources, value)
		}
	}
	seed, seedErr := strconv.ParseInt(strings.TrimSpace(share.Seed), 10, 64)
	noise, noiseErr := strconv.ParseFloat(strings.TrimSpace(share.NoiseRate), 64)
	if recipient == "" || len(sources) == 0 || seedErr != nil || noiseErr != nil || noise < 0 || noise > 1 {
		share.Running = false
		share.Error = "Recipient, source IDs, a whole-number seed, and a noise rate from 0 to 1 are required."
		s.Model.SetShare(share)
		s.requestFrame()
		return
	}
	allVersionIDs := make([]string, 0, len(sources)+1)
	allVersionIDs = append(allVersionIDs, recipient)
	allVersionIDs = append(allVersionIDs, sources...)
	configSources := make([]SharingSource, 0, len(allVersionIDs))
	for i, versionID := range allVersionIDs {
		wormID := fmt.Sprintf("source-%d", i)
		if i == 0 {
			wormID = "recipient"
		}
		configSources = append(configSources, SharingSource{WormID: wormID, BrainVersionID: versionID})
	}
	request := ShareExperimentRequest{
		SharingConfig:      SharingConfig{Policy: share.Policy, Seed: seed, NoiseRate: noise, Sources: configSources},
		RecipientVersionID: recipient,
		SourceVersionIDs:   allVersionIDs,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, _, err := s.Client.ShareExperiment(ctx, request)
	share.Running = false
	if err != nil {
		share.Error = err.Error()
	} else {
		share.Result = &result
		share.Error = ""
	}
	s.Model.SetShare(share)
	s.requestFrame()
}

func (s *Shell) submitDirection(direction Direction) {
	if !s.actionInFlight.CompareAndSwap(false, true) {
		return
	}
	defer s.finishAction()
	v := s.Model.Snapshot()
	if v.HUD.Paused || v.Board.GameOver {
		return
	}
	if !IsLegalDirection(v.Board.Legal, direction) {
		return
	}
	if v.GameID == "" || v.Board.ActiveWorm == "" {
		s.Model.SetGameError(errors.New("no authoritative active worm is available"))
		s.requestFrame()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var response GameResponse
	var err error
	if v.Board.Pending != nil {
		pending := v.Board.Pending
		response, _, err = s.Client.Teach(ctx, v.GameID, TeachRequest{Cursor: v.GameCursor, EventHash: v.EventHash, WormID: pending.WormID, Direction: int(direction), Mask: pending.Mask, Request: pending.Request, PendingMask: pending.Mask, PendingRequest: pending.Request})
	} else {
		response, _, err = s.Client.Act(ctx, v.GameID, ActRequest{Cursor: v.GameCursor, EventHash: v.EventHash, WormID: v.Board.ActiveWorm, Direction: int(direction)})
	}
	if err != nil {
		s.Model.SetGameError(fmt.Errorf("direction %s was not committed: %w", direction, err))
		s.requestFrame()
		return
	}
	s.Model.SetGame(v.GameID, response)
	persistGame(v.GameID)
	s.scheduler.Reset()
	s.requestFrame()
}

func (s *Shell) requestPause(paused bool) {
	s.pauseTarget.Store(paused)
	s.pauseRequested.Store(true)
	if s.actionInFlight.CompareAndSwap(false, true) {
		go s.finishAction()
	}
	s.requestFrame()
}

func (s *Shell) requestAbort() {
	s.abortRequested.Store(true)
	if s.actionInFlight.CompareAndSwap(false, true) {
		go s.finishAction()
	}
	s.requestFrame()
}

func (s *Shell) commitAbort() {
	v := s.Model.Snapshot()
	if v.GameID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, _, err := s.Client.Abort(ctx, v.GameID, GameCommandRequest{Cursor: v.GameCursor, EventHash: v.EventHash}); err != nil {
		s.Model.SetGameError(fmt.Errorf("game was not aborted: %w", err))
		s.requestFrame()
		return
	}
	s.Model.resetGame()
	persistGame("")
	s.scheduler.Reset()
	s.requestFrame()
}

func (s *Shell) commitPaused(paused bool) {
	v := s.Model.Snapshot()
	if v.GameID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	response, _, err := s.Client.Pause(ctx, v.GameID, GameCommandRequest{Cursor: v.GameCursor, EventHash: v.EventHash}, paused)
	if err != nil {
		s.Model.SetGameError(fmt.Errorf("pause state was not committed: %w", err))
		s.requestFrame()
		return
	}
	if response.State.Width > 0 {
		s.Model.SetGame(v.GameID, response)
	} else {
		s.Model.SetGameCommand(response.Game, paused)
	}
	s.scheduler.Reset()
	s.requestFrame()
}

// finishAction retains exclusive ownership while draining abort and pause
// intent. This makes those controls durable across an in-flight tick or
// teaching request, and prevents the scheduler from arming work between an
// action and its queued authoritative command.
func (s *Shell) finishAction() {
	for {
		if s.abortRequested.Swap(false) {
			s.pauseRequested.Store(false)
			s.commitAbort()
			continue
		}
		if s.pauseRequested.Swap(false) {
			s.commitPaused(s.pauseTarget.Load())
			continue
		}
		s.actionInFlight.Store(false)
		// Close the race with a request arriving after the swap but before the
		// action lock was released. The winner owns the same drain loop.
		pending := s.abortRequested.Load() || s.pauseRequested.Load()
		if !pending || !s.actionInFlight.CompareAndSwap(false, true) {
			s.requestFrame()
			return
		}
	}
}

func (s *Shell) scheduleAutonomous(gtx layout.Context) {
	v := s.Model.Snapshot()
	autonomous := v.Screen == ScreenPlay && v.GameID != "" && needsAuthoritativeTick(v.Board)
	identity := fmt.Sprintf("%s:%d:%s", v.GameID, v.GameCursor, v.EventHash)
	due, next := s.scheduler.Due(gtx.Now, v.HUD.Speed, v.HUD.Paused || s.pauseRequested.Load() || s.actionInFlight.Load(), autonomous, identity)
	if !next.IsZero() {
		gtx.Execute(op.InvalidateCmd{At: next})
	}
	if due {
		go s.submitAutonomous()
	}
}
func (s *Shell) submitAutonomous() {
	if !s.actionInFlight.CompareAndSwap(false, true) {
		return
	}
	defer s.finishAction()
	v := s.Model.Snapshot()
	if v.GameID == "" || v.HUD.Paused || v.Board.Pending != nil || v.Board.GameOver {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	response, _, err := s.Client.Tick(ctx, v.GameID, GameCommandRequest{Cursor: v.GameCursor, EventHash: v.EventHash})
	if err != nil {
		s.Model.SetGameError(fmt.Errorf("autonomous tick was not committed: %w", err))
		s.requestFrame()
		return
	}
	s.Model.SetGame(v.GameID, response)
	persistGame(v.GameID)
	s.requestFrame()
}
