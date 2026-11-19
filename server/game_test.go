package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGameImmich records trash/restore calls and the API keys used.
type fakeGameImmich struct {
	mu       sync.Mutex
	trashed  []string
	restored []string
	keys     []string
	failNext bool
}

func newFakeGameImmich(t *testing.T, f *fakeGameImmich) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("x-api-key"))
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/search/random":
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": "a1", "type": "IMAGE", "originalFileName": "one.jpg"},
				{"id": "a2", "type": "IMAGE", "originalFileName": "two.jpg"},
				{"id": "a3", "type": "IMAGE", "originalFileName": "three.jpg"},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/assets":
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.failNext {
				f.failNext = false
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body struct{ IDs []string }
			json.NewDecoder(r.Body).Decode(&body)
			f.trashed = append(f.trashed, body.IDs...)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/trash/restore/assets":
			var body struct{ IDs []string }
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.restored = append(f.restored, body.IDs...)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/api/assets/") && strings.HasSuffix(r.URL.Path, "/thumbnail"):
			w.Header().Set("Content-Type", "image/jpeg")
			io.WriteString(w, "JPEG:"+r.URL.Query().Get("size"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

type gameFixture struct {
	srv    *Server
	immich *fakeGameImmich
	alice  string
	bob    string
	carol  string
}

func newGameFixture(t *testing.T) *gameFixture {
	t.Helper()
	f := &fakeGameImmich{}
	ts := newFakeGameImmich(t, f)
	srv := NewServer(Config{ServerURL: ts.URL})
	srv.accounts.SetPassword(ts.URL, "alice", "alice-key", "password-alice")
	srv.accounts.SetPassword(ts.URL, "bob", "bob-key", "password-bob")
	srv.accounts.SetPassword(ts.URL, "carol", "carol-key", "password-carol")
	return &gameFixture{
		srv:    srv,
		immich: f,
		alice:  srv.session.CreateAPIKey("alice", "alice-key", ts.URL),
		bob:    srv.session.CreateAPIKey("bob", "bob-key", ts.URL),
		carol:  srv.session.CreateAPIKey("carol", "carol-key", ts.URL),
	}
}

func (fx *gameFixture) do(t *testing.T, token, method, path, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	fx.srv.ServeHTTP(rr, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

func (fx *gameFixture) create(t *testing.T) string {
	t.Helper()
	rr, out := fx.do(t, fx.alice, http.MethodPost, "/api/game", `{"name":"Spring cleaning","partner":"bob","count":3}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	return out["id"].(string)
}

func (fx *gameFixture) vote(t *testing.T, token, id, asset, vote string) string {
	t.Helper()
	rr, out := fx.do(t, token, http.MethodPost, "/api/game/"+id+"/vote", `{"assetId":"`+asset+`","vote":"`+vote+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("vote %s %s: %d %s", asset, vote, rr.Code, rr.Body.String())
	}
	return out["outcome"].(string)
}

func TestGame_CreateValidatesPartner(t *testing.T) {
	fx := newGameFixture(t)
	rr, out := fx.do(t, fx.alice, http.MethodPost, "/api/game", `{"partner":"nobody"}`)
	if rr.Code != http.StatusBadRequest || out["code"] != "unknown_partner" {
		t.Fatalf("expected unknown_partner, got %d %v", rr.Code, out)
	}
	rr, out = fx.do(t, fx.alice, http.MethodPost, "/api/game", `{"partner":"alice"}`)
	if rr.Code != http.StatusBadRequest || out["code"] != "partner_required" {
		t.Fatalf("expected partner_required, got %d %v", rr.Code, out)
	}
}

func TestGame_FullRound(t *testing.T) {
	fx := newGameFixture(t)
	id := fx.create(t)

	// Both players see all three photos.
	for _, tok := range []string{fx.alice, fx.bob} {
		rr, out := fx.do(t, tok, http.MethodGet, "/api/game/"+id+"?limit=10", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("state: %d %s", rr.Code, rr.Body.String())
		}
		if n := len(out["next"].([]interface{})); n != 3 {
			t.Fatalf("expected 3 next assets, got %d", n)
		}
	}
	// Non-players cannot see the game.
	if rr, _ := fx.do(t, fx.carol, http.MethodGet, "/api/game/"+id, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("carol should get 404, got %d", rr.Code)
	}

	// Agreement: keep.
	if got := fx.vote(t, fx.alice, id, "a1", "keep"); got != "waiting" {
		t.Fatalf("expected waiting, got %s", got)
	}
	// Bob must not see Alice's pending vote.
	_, state := fx.do(t, fx.bob, http.MethodGet, "/api/game/"+id+"?limit=10", "")
	for _, raw := range state["next"].([]interface{}) {
		a := raw.(map[string]interface{})
		if _, leaked := a["votes"]; leaked {
			t.Fatalf("pending asset leaked votes: %v", a)
		}
	}
	if got := fx.vote(t, fx.bob, id, "a1", "keep"); got != "kept" {
		t.Fatalf("expected kept, got %s", got)
	}

	// Agreement: delete → trashed with the owner's key.
	fx.vote(t, fx.bob, id, "a2", "delete")
	if got := fx.vote(t, fx.alice, id, "a2", "delete"); got != "deleted" {
		t.Fatalf("expected deleted, got %s", got)
	}
	if len(fx.immich.trashed) != 1 || fx.immich.trashed[0] != "a2" {
		t.Fatalf("expected a2 trashed, got %v", fx.immich.trashed)
	}

	// Disagreement → disputed, nothing trashed.
	fx.vote(t, fx.alice, id, "a3", "keep")
	if got := fx.vote(t, fx.bob, id, "a3", "delete"); got != "disputed" {
		t.Fatalf("expected disputed, got %s", got)
	}
	if len(fx.immich.trashed) != 1 {
		t.Fatalf("disputed asset must not be trashed: %v", fx.immich.trashed)
	}
	for _, k := range fx.immich.keys {
		if k != "alice-key" {
			t.Fatalf("all Immich calls must use the owner's key, saw %q", k)
		}
	}

	// Re-voting a decided asset is refused.
	if rr, _ := fx.do(t, fx.bob, http.MethodPost, "/api/game/"+id+"/vote", `{"assetId":"a1","vote":"delete"}`); rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}

	// Only the owner reviews.
	if rr, _ := fx.do(t, fx.bob, http.MethodGet, "/api/game/"+id+"/review", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("bob review: expected 403, got %d", rr.Code)
	}
	rr, review := fx.do(t, fx.alice, http.MethodGet, "/api/game/"+id+"/review", "")
	if rr.Code != http.StatusOK || len(review["items"].([]interface{})) != 1 {
		t.Fatalf("review: %d %v", rr.Code, review)
	}
	rr, _ = fx.do(t, fx.alice, http.MethodPost, "/api/game/"+id+"/resolve", `{"assetId":"a3","decision":"delete"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rr.Code, rr.Body.String())
	}
	if len(fx.immich.trashed) != 2 || fx.immich.trashed[1] != "a3" {
		t.Fatalf("expected a3 trashed after admin decision, got %v", fx.immich.trashed)
	}
	// Admin decisions cannot be undone by a player.
	if rr, _ := fx.do(t, fx.bob, http.MethodPost, "/api/game/"+id+"/undo", `{"assetId":"a3"}`); rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for undoing admin decision, got %d", rr.Code)
	}
}

func TestGame_UndoRestoresTrashedAsset(t *testing.T) {
	fx := newGameFixture(t)
	id := fx.create(t)
	fx.vote(t, fx.alice, id, "a1", "delete")
	fx.vote(t, fx.bob, id, "a1", "delete")
	rr, out := fx.do(t, fx.bob, http.MethodPost, "/api/game/"+id+"/undo", `{"assetId":"a1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rr.Code, rr.Body.String())
	}
	if len(fx.immich.restored) != 1 || fx.immich.restored[0] != "a1" {
		t.Fatalf("expected a1 restored, got %v", fx.immich.restored)
	}
	asset := out["asset"].(map[string]interface{})
	if asset["status"] != "pending" || asset["myVote"] != "" {
		t.Fatalf("expected pending without bob's vote, got %v", asset)
	}
}

func TestGame_TrashFailureGoesToReview(t *testing.T) {
	fx := newGameFixture(t)
	id := fx.create(t)
	fx.immich.failNext = true
	fx.vote(t, fx.alice, id, "a1", "delete")
	if got := fx.vote(t, fx.bob, id, "a1", "delete"); got != "error" {
		t.Fatalf("expected error outcome, got %s", got)
	}
	_, review := fx.do(t, fx.alice, http.MethodGet, "/api/game/"+id+"/review", "")
	items := review["items"].([]interface{})
	if len(items) != 1 || items[0].(map[string]interface{})["status"] != "error" {
		t.Fatalf("expected failed delete in review, got %v", items)
	}
	rr, _ := fx.do(t, fx.alice, http.MethodPost, "/api/game/"+id+"/resolve", `{"assetId":"a1","decision":"delete"}`)
	if rr.Code != http.StatusOK || len(fx.immich.trashed) != 1 {
		t.Fatalf("retry delete: %d trashed=%v", rr.Code, fx.immich.trashed)
	}
}

func TestGame_MediaScopedToGame(t *testing.T) {
	fx := newGameFixture(t)
	id := fx.create(t)

	rr, _ := fx.do(t, fx.bob, http.MethodGet, "/api/game/"+id+"/assets/a1/thumbnail?size=preview", "")
	if rr.Code != http.StatusOK || rr.Body.String() != "JPEG:preview" {
		t.Fatalf("partner thumbnail: %d %q", rr.Code, rr.Body.String())
	}
	last := fx.immich.keys[len(fx.immich.keys)-1]
	if last != "alice-key" {
		t.Fatalf("media must use the owner's key, got %q", last)
	}
	if rr, _ := fx.do(t, fx.bob, http.MethodGet, "/api/game/"+id+"/assets/zzz/thumbnail", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("asset outside game: expected 404, got %d", rr.Code)
	}
	if rr, _ := fx.do(t, fx.carol, http.MethodGet, "/api/game/"+id+"/assets/a1/thumbnail", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("non-player: expected 404, got %d", rr.Code)
	}
}

func TestGame_PlayersGetDifferentOrders(t *testing.T) {
	g := &Game{Players: []string{"a", "b"}, Seeds: map[string]int64{"a": 1, "b": 2}}
	for i := 0; i < 50; i++ {
		g.Assets = append(g.Assets, &GameAsset{ID: string(rune('A' + i)), Status: gameStatusPending})
	}
	g.rebuild()
	same := true
	for i := range g.order["a"] {
		if g.order["a"][i] != g.order["b"][i] {
			same = false
		}
	}
	if same {
		t.Fatal("expected different orders per player")
	}
}

func TestGame_EmailsOwnerWhenFinished(t *testing.T) {
	fx := newGameFixture(t)
	fx.srv.config.SMTPHost = "smtp.example"
	fx.srv.accounts.SetEmail(fx.srv.config.ServerURL, "alice", "alice@example.com")
	var mu sync.Mutex
	var subjects []string
	done := make(chan struct{}, 4)
	orig := sendMailFunc
	sendMailFunc = func(_ EmailSettings, to, subject, _ string) error {
		mu.Lock()
		if to == "alice@example.com" {
			subjects = append(subjects, subject)
		}
		mu.Unlock()
		done <- struct{}{}
		return nil
	}
	t.Cleanup(func() { sendMailFunc = orig })

	id := fx.create(t)
	for _, a := range []string{"a1", "a2", "a3"} {
		fx.vote(t, fx.alice, id, a, "keep")
		fx.vote(t, fx.bob, id, a, "delete")
	}
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(subjects) != 1 || !strings.Contains(subjects[0], "finished") {
		t.Fatalf("expected one finished email to owner, got %v", subjects)
	}
}
