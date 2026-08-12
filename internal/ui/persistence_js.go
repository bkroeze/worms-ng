//go:build js && wasm

package ui

import "syscall/js"

const activeGameStorageKey = "worms-ng.active-game.v1"

func loadPersistedGame() string {
	storage := js.Global().Get("localStorage")
	if storage.IsUndefined() || storage.IsNull() {
		return ""
	}
	value := storage.Call("getItem", activeGameStorageKey)
	if value.IsNull() || value.IsUndefined() {
		return ""
	}
	return value.String()
}

func persistGame(id string) {
	storage := js.Global().Get("localStorage")
	if storage.IsUndefined() || storage.IsNull() {
		return
	}
	if id == "" {
		storage.Call("removeItem", activeGameStorageKey)
		return
	}
	storage.Call("setItem", activeGameStorageKey, id)
}
