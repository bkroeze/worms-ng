//go:build js && wasm

package ui

import (
	"encoding/json"
	"syscall/js"
	"testing"
)

func TestBrowserHooksExposeSetupNavigationAndAuthoritativeState(t *testing.T) {
	shell := NewShell("")
	shell.Model.SetBoard(BoardView{
		Width: 4, Height: 4,
		Territory:       map[Point]uint32{{X: 1, Y: 2}: 0x123456ff},
		TerritoryOwners: map[Point]string{{X: 1, Y: 2}: "arbitrary-owner"},
		Capture:         CaptureView{Points: map[Point]uint32{{X: 1, Y: 2}: 0x123456ff}},
	})
	installBrowserHooks(shell)
	hooks := js.Global().Get("__wormsTest")
	if hooks.Type() != js.TypeObject || hooks.Get("snapshot").Type() != js.TypeFunction {
		t.Fatalf("browser hook object was not installed: %v", hooks)
	}
	if !hooks.Get("ready").InstanceOf(js.Global().Get("Promise")) {
		t.Fatal("ready is not a promise")
	}

	input := js.Global().Get("Object").New()
	input.Set("slotCount", 1)
	input.Set("width", 12)
	input.Set("height", 10)
	input.Set("seed", "99")
	slot := js.Global().Get("Object").New()
	slot.Set("id", "arbitrary/worm:id")
	slot.Set("name", "Browser Worm")
	slot.Set("controller", ControllerNew)
	slot.Set("x", 2)
	slot.Set("y", 3)
	slots := js.Global().Get("Array").New()
	slots.Call("push", slot)
	input.Set("slots", slots)
	hooks.Get("setSetup").Invoke(input)
	setup := shell.Model.Snapshot().Setup
	if setup.SlotCount != 1 || setup.Width != 12 || setup.Height != 10 || setup.Seed != "99" || setup.Slots[0].ID != "arbitrary/worm:id" || setup.Slots[0].Start != (Point{X: 2, Y: 3}) {
		t.Fatalf("setSetup hook lost fields: %+v", setup)
	}

	hooks.Get("navigate").Invoke("brains")
	state := hooks.Get("snapshot").Invoke()
	if state.Get("screen").String() != "brains" || !state.Get("ready").Bool() {
		t.Fatalf("snapshot navigation state = %v", state)
	}
	capture := state.Get("capture")
	if capture.Length() != 1 || uint32(capture.Index(0).Get("color").Int()) != 0x123456ff {
		t.Fatalf("snapshot lost authoritative capture color: %v", capture)
	}
}

func TestWASMFogResponseDecodesWithoutAuthoritativeState(t *testing.T) {
	const payload = `{
		"game":{"id":"fog-wasm","status":"completed","width":4,"height":3,"tick":12,
			"participants":[{"id":"a","name":"Ada"},{"id":"b","name":"Babbage"}]},
		"extension":{
			"config":{"version":1,"enabled":true,"width":4,"height":3,"fog_of_war":true},
			"observation":{"version":1,"worm_id":"a",
				"base":{"version":1,"worm_id":"a","position":{"q":1,"r":1},"legal":[1,5],"scores":[9]},
				"visible":[{"point":{"q":1,"r":1},"visible":true},{"point":{"q":2,"r":1},"visible":false}],
				"unknown_count":1},
			"scores":{"a":9,"b":4},"winners":["a"]
		}
	}`
	var response GameResponse
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatal(err)
	}
	shell := NewShell("")
	shell.Model.SetGame("fog-wasm", response)
	view := shell.Model.Snapshot()
	if view.Board.Width != 4 || view.Board.Height != 3 || len(view.Board.Worms) != 1 || view.Board.Worms[0].ID != "a" {
		t.Fatalf("redacted response did not produce a fog-safe board: %+v", view.Board)
	}
	if !view.Board.Legal[1] || !view.Board.Legal[5] || view.Board.Legal[0] {
		t.Fatalf("typed observation legal moves were not consumed: %v", view.Board.Legal)
	}
	if len(view.HUD.Scores) != 2 || view.HUD.Scores[0].Score != 9 || len(view.HUD.Winners) != 1 || view.HUD.Winners[0] != "Ada" {
		t.Fatalf("completion result was not projected in WASM: %+v", view.HUD)
	}

	installBrowserHooks(shell)
	state := js.Global().Get("__wormsTest").Get("snapshot").Invoke()
	if state.Get("winners").Length() != 1 || state.Get("winners").Index(0).String() != "Ada" {
		t.Fatalf("browser snapshot omitted completion winner: %v", state)
	}
}
