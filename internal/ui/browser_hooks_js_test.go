//go:build js && wasm

package ui

import (
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
