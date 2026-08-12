package ui

import (
	"image"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
)

func TestBrainsClickDispatchesBeforeButtonLayout(t *testing.T) {
	requests := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1","brains":[{"id":"b1","name":"Navigator","version":"v1"}]}`))
	}))
	defer srv.Close()

	shell := NewShell(srv.URL)
	shell.brains.Click()
	var ops op.Ops
	gtx := layout.Context{Ops: &ops, Constraints: layout.Exact(image.Pt(1280, 577))}

	// Nav dispatches before laying out material buttons; otherwise Gio's
	// button layout drains pending events as part of its update pass.
	shell.nav(gtx)

	if got := shell.Model.Snapshot().Screen; got != ScreenBrains {
		t.Fatalf("screen = %q, want %q", got, ScreenBrains)
	}
	select {
	case got := <-requests:
		if got != "GET /api/v1/brains" {
			t.Fatalf("request = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("brains navigation did not load the API resource")
	}
}
