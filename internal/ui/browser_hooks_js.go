//go:build js && wasm

package ui

import (
	"encoding/json"
	"strconv"
	"strings"
	"syscall/js"
	"time"
)

var browserHookFuncs []js.Func

type browserSetupSlotInput struct {
	ID         *string `json:"id"`
	Name       *string `json:"name"`
	Controller *string `json:"controller"`
	BrainID    *string `json:"brainID"`
	Color      *string `json:"color"`
	X          *int    `json:"x"`
	Y          *int    `json:"y"`
}

type browserSetupInput struct {
	SlotCount *int                    `json:"slotCount"`
	Width     *int                    `json:"width"`
	Height    *int                    `json:"height"`
	Seed      *string                 `json:"seed"`
	Ruleset   *string                 `json:"ruleset"`
	Slots     []browserSetupSlotInput `json:"slots"`
}

type browserInspectInput struct {
	BrainID *string `json:"brainID"`
	Version *int    `json:"version"`
	Filter  *string `json:"filter"`
	Offset  *int    `json:"offset"`
	Limit   *int    `json:"limit"`
}

type browserToggleInput struct {
	Grid          *bool `json:"grid"`
	Flash         *bool `json:"flash"`
	ReducedMotion *bool `json:"reducedMotion"`
}

func installBrowserHooks(shell *Shell) {
	hooks := js.Global().Get("Object").New()
	bindBrowserHook(hooks, "snapshot", func(_ js.Value, _ []js.Value) any {
		return browserJSONValue(shell.browserState())
	})
	bindBrowserHook(hooks, "setSetup", func(_ js.Value, args []js.Value) any {
		var input browserSetupInput
		if decodeBrowserArgument(args, &input) {
			setup := shell.Model.Snapshot().Setup
			applyBrowserSetup(&setup, input)
			shell.Model.SetSetup(setup)
			shell.requestFrame()
		}
		return browserJSONValue(shell.browserState())
	})
	bindBrowserHook(hooks, "setToggles", func(_ js.Value, args []js.Value) any {
		var input browserToggleInput
		if decodeBrowserArgument(args, &input) {
			view := shell.Model.Snapshot()
			if input.Grid != nil && view.Toggles.Grid != *input.Grid {
				shell.Model.ToggleGrid()
			}
			if input.Flash != nil && view.Toggles.Flash != *input.Flash {
				shell.Model.ToggleFlash()
			}
			if input.ReducedMotion != nil && view.Toggles.ReducedMotion != *input.ReducedMotion {
				shell.Model.ToggleReducedMotion()
			}
			shell.requestFrame()
		}
		return browserJSONValue(shell.browserState())
	})
	bindBrowserHook(hooks, "navigate", func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			if screen, ok := browserScreen(args[0].String()); ok {
				shell.Model.Navigate(screen)
				shell.requestFrame()
			}
		}
		return browserJSONValue(shell.browserState())
	})
	bindBrowserHook(hooks, "start", func(_ js.Value, _ []js.Value) any {
		return browserPromise(func() { shell.startGame() }, shell)
	})
	directionHook := func(_ js.Value, args []js.Value) any {
		return browserPromise(func() {
			if direction, ok := browserDirection(args); ok {
				shell.submitDirection(direction)
			}
		}, shell)
	}
	bindBrowserHook(hooks, "direction", directionHook)
	bindBrowserHook(hooks, "teach", directionHook)
	bindBrowserHook(hooks, "tick", func(_ js.Value, _ []js.Value) any {
		return browserPromise(func() { shell.submitAutonomous() }, shell)
	})
	bindBrowserHook(hooks, "pause", func(_ js.Value, _ []js.Value) any {
		return browserPromise(func() {
			shell.requestPause(true)
			waitForBrowserIdle(shell)
		}, shell)
	})
	bindBrowserHook(hooks, "resume", func(_ js.Value, _ []js.Value) any {
		return browserPromise(func() {
			shell.requestPause(false)
			waitForBrowserIdle(shell)
		}, shell)
	})
	bindBrowserHook(hooks, "abort", func(_ js.Value, _ []js.Value) any {
		return browserPromise(func() {
			shell.requestAbort()
			waitForBrowserIdle(shell)
		}, shell)
	})
	bindBrowserHook(hooks, "inspect", func(_ js.Value, args []js.Value) any {
		return browserPromise(func() {
			query := shell.Model.Snapshot().Inspect
			var input browserInspectInput
			if decodeBrowserArgument(args, &input) {
				if input.Offset == nil && (input.BrainID != nil || input.Version != nil || input.Filter != nil) {
					query.Offset = 0
				}
				if input.BrainID != nil {
					query.BrainID = *input.BrainID
				}
				if input.Version != nil {
					query.Version = strconv.Itoa(*input.Version)
				}
				if input.Filter != nil {
					query.Filter = *input.Filter
				}
				if input.Offset != nil {
					query.Offset = *input.Offset
				}
				if input.Limit != nil {
					query.Limit = *input.Limit
				}
			}
			shell.Model.SetInspectorQuery(query)
			shell.loadInspector()
		}, shell)
	})
	hooks.Set("ready", js.Global().Get("Promise").Call("resolve", browserJSONValue(shell.browserState())))
	js.Global().Set("__wormsTest", hooks)
}

func bindBrowserHook(target js.Value, name string, callback func(js.Value, []js.Value) any) {
	fn := js.FuncOf(callback)
	browserHookFuncs = append(browserHookFuncs, fn)
	target.Set(name, fn)
}

func browserPromise(action func(), shell *Shell) js.Value {
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve := args[0]
		go func() {
			action()
			resolve.Invoke(browserJSONValue(shell.browserState()))
		}()
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	return promise
}

func browserJSONValue(value any) js.Value {
	encoded, err := json.Marshal(value)
	if err != nil {
		return js.Null()
	}
	return js.Global().Get("JSON").Call("parse", string(encoded))
}

func decodeBrowserArgument(args []js.Value, destination any) bool {
	if len(args) == 0 || args[0].IsNull() || args[0].IsUndefined() {
		return false
	}
	encoded := js.Global().Get("JSON").Call("stringify", args[0])
	return encoded.Type() == js.TypeString && json.Unmarshal([]byte(encoded.String()), destination) == nil
}

func applyBrowserSetup(setup *SetupConfig, input browserSetupInput) {
	if input.SlotCount != nil {
		setup.SlotCount = *input.SlotCount
	}
	if input.Width != nil {
		setup.Width = *input.Width
	}
	if input.Height != nil {
		setup.Height = *input.Height
	}
	if input.Seed != nil {
		setup.Seed = *input.Seed
	}
	if input.Ruleset != nil {
		setup.Ruleset = *input.Ruleset
	}
	for index := range min(len(input.Slots), len(setup.Slots)) {
		inputSlot, slot := input.Slots[index], &setup.Slots[index]
		if inputSlot.ID != nil {
			slot.ID = *inputSlot.ID
		}
		if inputSlot.Name != nil {
			slot.Name = *inputSlot.Name
		}
		if inputSlot.Controller != nil {
			slot.Controller = *inputSlot.Controller
		}
		if inputSlot.BrainID != nil {
			slot.BrainID = *inputSlot.BrainID
		}
		if inputSlot.Color != nil {
			slot.Color = *inputSlot.Color
		}
		if inputSlot.X != nil {
			slot.Start.X = *inputSlot.X
		}
		if inputSlot.Y != nil {
			slot.Start.Y = *inputSlot.Y
		}
	}
}

func browserDirection(args []js.Value) (Direction, bool) {
	if len(args) == 0 {
		return 0, false
	}
	if args[0].Type() == js.TypeNumber {
		direction := args[0].Int()
		return Direction(direction), direction >= int(East) && direction <= int(NorthEast)
	}
	value := strings.ToUpper(strings.TrimSpace(args[0].String()))
	for direction, name := range directionNames {
		if value == name {
			return Direction(direction), true
		}
	}
	return DirectionFromKey(value)
}

func browserScreen(value string) (Screen, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "setup":
		return ScreenSetup, true
	case "play":
		return ScreenPlay, true
	case "games":
		return ScreenGames, true
	case "brains":
		return ScreenBrains, true
	case "inspector":
		return ScreenInspector, true
	case "tournament":
		return ScreenTournament, true
	default:
		return ScreenSetup, false
	}
}

func waitForBrowserIdle(shell *Shell) {
	deadline := time.Now().Add(10 * time.Second)
	for (shell.actionInFlight.Load() || shell.pauseRequested.Load()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
