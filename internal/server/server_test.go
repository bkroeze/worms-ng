package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"worms.ng/internal/engine"
	"worms.ng/internal/protocol"
)

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>worms</title>")},
		"main.wasm":  &fstest.MapFile{Data: []byte("wasm")},
		"wasm.js":    &fstest.MapFile{Data: []byte("loader")},
	}
}

func TestVersionedEndpointsUseSQLiteAndServeEmbeddedAssets(t *testing.T) {
	svc, err := Open(":memory:", testAssets())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	h := svc.Handler()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", recorder.Code)
	}
	var health protocol.Health
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.Version != protocol.APIVersion || health.Status != "ok" || health.Demo.Database != "sqlite" || health.Demo.RecordedChecks != 1 {
		t.Fatalf("unexpected health payload: %+v", health)
	}
	for _, path := range []string{"/", "/main.wasm", "/wasm.js"} {
		recorder = httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
			t.Fatalf("%s response: status=%d body=%q", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestGameContractAndRestartResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "worms.sqlite")
	body := `{"version":"v1","id":"g1","participants":[{"id":"w1","name":"one","kind":"human"}]}`
	service, err := Open(db, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		service.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := post("/api/v1/games", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create game status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	service.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/games", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"g1"`) {
		t.Fatalf("list games status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = post("/api/v1/games/g1/act", `{"version":"v1","cursor":0,"worm_id":"w1","direction":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("act status=%d body=%s", rec.Code, rec.Body.String())
	}
	var acted struct {
		Game struct {
			Sequence  int64  `json:"sequence"`
			EventHash string `json:"event_hash"`
		} `json:"game"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &acted); err != nil || acted.Game.Sequence != 1 || acted.Game.EventHash == "" {
		t.Fatalf("bad act response: %s", rec.Body.String())
	}
	service.Close()
	service, err = Open(db, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	rec = httptest.NewRecorder()
	service.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/games/g1/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resumed struct {
		State struct {
			Tick uint64 `json:"tick"`
		} `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resumed); err != nil || resumed.State.Tick == 0 {
		t.Fatalf("resume status=%d headers=%v did not restore snapshot: %s", rec.Code, rec.Header(), rec.Body.String())
	}
	rec = post("/api/v1/games/g1/act", `{"version":"v1","cursor":0,"worm_id":"w1","direction":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale act status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBrainTournamentContracts(t *testing.T) {
	svc, err := Open(":memory:", testAssets())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	call := func(method, path, payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		svc.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodPost, "/api/v1/brains", `{"version":"v1","id":"b1","name":"brain"}`); rec.Code != http.StatusCreated {
		t.Fatalf("brain=%d %s", rec.Code, rec.Body.String())
	}
	payload := `{"version":"v1","number":1,"rules":{"version":1,"data":{}},"lineage":{"version":1,"data":{}},"provenance":{"version":1,"data":{}},"payload":{"version":1,"data":{}}}`
	if rec := call(http.MethodPost, "/api/v1/brains/b1/versions", payload); rec.Code != http.StatusCreated {
		t.Fatalf("brain version=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/v1/tournaments", `{"version":"v1","id":"t1","name":"cup"}`); rec.Code != http.StatusCreated {
		t.Fatalf("tournament=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/v1/tournaments/t1/matches", `{"version":"v1","id":"m1","round":1}`); rec.Code != http.StatusCreated {
		t.Fatalf("match=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, "/api/v1/tournaments/t1/matches", ``); rec.Code != http.StatusOK {
		t.Fatalf("matches=%d %s", rec.Code, rec.Body.String())
	}
}

func TestCORSAllowlistAndETag(t *testing.T) {
	svc, err := OpenWithOptions(":memory:", testAssets(), Options{CORSOrigins: []string{"https://example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Origin", "https://example.test")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://example.test" || rec.Header().Get("ETag") == "" {
		t.Fatalf("missing CORS/etag headers: %v", rec.Header())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("etag status=%d", rec2.Code)
	}
}

func TestMutationRequiresExplicitAPIVersion(t *testing.T) {
	svc, err := Open(":memory:", testAssets())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	call := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		svc.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := call("/api/v1/games", `{"id":"missing-version"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_version") {
		t.Fatalf("missing game version: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := call("/api/v1/brains", `{"id":"missing-version"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_version") {
		t.Fatalf("missing brain version: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTeachAppliesDecisionAndSurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "teach.sqlite")
	createBody, _ := json.Marshal(map[string]any{
		"version": "v1", "id": "teach-game",
		"participants": []map[string]any{{"id": "w1", "name": "one", "kind": "human"}},
	})
	service, err := Open(db, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, body []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		service.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/api/v1/games", createBody); rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	tickBody := []byte(`{"version":"v1","cursor":0,"event_hash":""}`)
	rec := post("/api/v1/games/teach-game/tick", tickBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("tick status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ticked struct {
		Game struct {
			Cursor    int64  `json:"cursor"`
			EventHash string `json:"event_hash"`
		} `json:"game"`
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ticked); err != nil {
		t.Fatal(err)
	}
	pendingState, err := engine.UnmarshalSnapshot(ticked.State)
	if err != nil || pendingState.Pending == nil {
		t.Fatalf("pending state=%+v err=%v", pendingState.Pending, err)
	}
	teachBody, _ := json.Marshal(map[string]any{
		"version": "v1", "cursor": ticked.Game.Cursor, "event_hash": ticked.Game.EventHash, "worm_id": "w1",
		"mask": pendingState.Pending.Mask, "request": pendingState.Pending.Request, "direction": int(engine.East),
	})
	rec = post("/api/v1/games/teach-game/teach", teachBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("teach status=%d body=%s", rec.Code, rec.Body.String())
	}
	var taught struct {
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &taught); err != nil {
		t.Fatal(err)
	}
	taughtState, err := engine.UnmarshalSnapshot(taught.State)
	if err != nil {
		t.Fatal(err)
	}
	if taughtState.Worms[0].Rules[0] != engine.Action(engine.East) {
		t.Fatalf("teach did not apply: pending=%+v rule=%v", taughtState.Pending, taughtState.Worms[0].Rules[0])
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service, err = Open(db, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	rec = httptest.NewRecorder()
	service.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/games/teach-game/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resumed struct {
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resumed); err != nil {
		t.Fatal(err)
	}
	resumedState, err := engine.UnmarshalSnapshot(resumed.State)
	if err != nil {
		t.Fatal(err)
	}
	if resumedState.Worms[0].Rules[0] != engine.Action(engine.East) {
		t.Fatalf("restart lost taught state: pending=%+v rule=%v", resumedState.Pending, resumedState.Worms[0].Rules[0])
	}
}

func TestConcurrentActionsHaveSingleWinner(t *testing.T) {
	svc, err := Open(":memory:", testAssets())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	create := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/games", strings.NewReader(`{"version":"v1","id":"concurrent","participants":[{"id":"w1"}]}`))
	req.Header.Set("Content-Type", "application/json")
	svc.Handler().ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/games/concurrent/act", strings.NewReader(`{"version":"v1","cursor":0,"worm_id":"w1","direction":0}`))
			req.Header.Set("Content-Type", "application/json")
			svc.Handler().ServeHTTP(rec, req)
			statuses <- rec.Code
		}()
	}
	wg.Wait()
	close(statuses)
	var ok, conflict int
	for status := range statuses {
		if status == http.StatusOK {
			ok++
		} else if status == http.StatusConflict {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("concurrent action statuses: ok=%d conflict=%d", ok, conflict)
	}
}
