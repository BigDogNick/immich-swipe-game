package main

// ─── Couples Game ──────────────────────────────────────────────────────────
//
// A game is a fixed pool of assets from the owner's Immich library that two
// (or more) Swipe accounts on the same Immich server review independently.
// Every player sees the same assets, each in their own shuffled order. Votes
// are hidden from the other players until everyone has voted on an asset;
// then the asset resolves:
//
//   - everyone kept it    → kept (nothing changes in Immich)
//   - everyone deleted it → moved to the Immich trash with the owner's key
//   - mixed votes         → disputed, waits for the owner (admin) to decide
//
// The owner is the game's admin. Because a deletion needs every player's vote
// (or the owner's own decision), a partner can never delete an owner's photo
// alone. Partners view the photos through game-scoped media endpoints that
// use the owner's credentials, so they need no access to the owner's library.
//
// Games are kept in memory and written through to the shared SQLite database
// (IMMICH_SESSIONS_DB) when one is configured, as one JSON row per game.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gameMaxAssets     = 2000
	gameDefaultAssets = 100
	gameMaxPlayers    = 4
	gameMaxNameLen    = 80
)

// Asset statuses.
const (
	gameStatusPending  = "pending"
	gameStatusKept     = "kept"
	gameStatusDeleted  = "deleted"
	gameStatusDisputed = "disputed"
	gameStatusError    = "error" // agreed/decided delete, but the Immich call failed
)

const (
	gameVoteKeep   = "keep"
	gameVoteDelete = "delete"
)

type GameAsset struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	FileName      string            `json:"originalFileName"`
	FileCreatedAt string            `json:"fileCreatedAt"`
	LocalDateTime string            `json:"localDateTime"`
	Votes         map[string]string `json:"votes"` // player → keep|delete
	Status        string            `json:"status"`
	DecidedBy     string            `json:"decidedBy,omitempty"` // "" | "agreement" | "admin"
	DecidedAt     int64             `json:"decidedAt,omitempty"`
	Error         string            `json:"error,omitempty"`
}

type Game struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	ServerURL   string           `json:"serverUrl"`
	Owner       string           `json:"owner"`
	Players     []string         `json:"players"` // owner first
	Seeds       map[string]int64 `json:"seeds"`
	CreatedAt   int64            `json:"createdAt"`
	Source      string           `json:"source"` // "random" | "album:<id>"
	Assets      []*GameAsset     `json:"assets"`
	NotifiedEnd bool             `json:"notifiedEnd"`

	index map[string]int   // asset id → position in Assets
	order map[string][]int // player → permutation of Assets indices
}

func (g *Game) rebuild() {
	g.index = make(map[string]int, len(g.Assets))
	for i, a := range g.Assets {
		g.index[a.ID] = i
		if a.Votes == nil {
			a.Votes = map[string]string{}
		}
	}
	g.order = make(map[string][]int, len(g.Players))
	for _, p := range g.Players {
		seed := uint64(g.Seeds[p])
		r := mrand.New(mrand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		g.order[p] = r.Perm(len(g.Assets))
	}
}

func (g *Game) hasPlayer(name string) bool {
	for _, p := range g.Players {
		if p == name {
			return true
		}
	}
	return false
}

func (g *Game) asset(id string) (*GameAsset, bool) {
	i, ok := g.index[id]
	if !ok {
		return nil, false
	}
	return g.Assets[i], true
}

// nextFor returns up to limit assets the player has not voted on yet, in the
// player's own order.
func (g *Game) nextFor(player string, limit int) []*GameAsset {
	out := make([]*GameAsset, 0, limit)
	for _, i := range g.order[player] {
		a := g.Assets[i]
		if _, voted := a.Votes[player]; voted {
			continue
		}
		if a.Status != gameStatusPending {
			// Decided by the admin before this player got to it.
			continue
		}
		out = append(out, a)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (g *Game) votedCount(player string) int {
	n := 0
	for _, a := range g.Assets {
		if _, ok := a.Votes[player]; ok || a.Status != gameStatusPending {
			n++
		}
	}
	return n
}

func (g *Game) allVoted(a *GameAsset) bool {
	for _, p := range g.Players {
		if _, ok := a.Votes[p]; !ok {
			return false
		}
	}
	return true
}

// tally returns the agreed decision ("keep"/"delete") or "" when votes differ.
func (g *Game) tally(a *GameAsset) string {
	decision := ""
	for _, p := range g.Players {
		v := a.Votes[p]
		if decision == "" {
			decision = v
		} else if v != decision {
			return ""
		}
	}
	return decision
}

func (g *Game) finished() bool {
	for _, a := range g.Assets {
		if a.Status == gameStatusPending {
			return false
		}
	}
	return true
}

func (g *Game) counts() map[string]int {
	c := map[string]int{
		gameStatusPending: 0, gameStatusKept: 0, gameStatusDeleted: 0,
		gameStatusDisputed: 0, gameStatusError: 0,
	}
	for _, a := range g.Assets {
		c[a.Status]++
	}
	return c
}

// ─── Store ─────────────────────────────────────────────────────────────────

type GameStore struct {
	mu    sync.Mutex
	db    *sql.DB
	games map[string]*Game
}

const gamesSchema = `
CREATE TABLE IF NOT EXISTS games (
	id         TEXT PRIMARY KEY,
	data       TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);`

func NewGameStore(db *sql.DB) *GameStore {
	s := &GameStore{db: db, games: make(map[string]*Game)}
	if db == nil {
		return s
	}
	if _, err := db.Exec(gamesSchema); err != nil {
		log.Printf("Warning: cannot migrate games table: %v (games in-memory only)", err)
		s.db = nil
		return s
	}
	rows, err := db.Query(`SELECT data FROM games`)
	if err != nil {
		log.Printf("Warning: cannot load games: %v", err)
		return s
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			continue
		}
		var g Game
		if err := json.Unmarshal([]byte(data), &g); err != nil {
			log.Printf("Warning: skipping unreadable game row: %v", err)
			continue
		}
		g.rebuild()
		s.games[g.ID] = &g
	}
	return s
}

// save persists a game. Callers must hold s.mu.
func (s *GameStore) save(g *Game) {
	if s.db == nil {
		return
	}
	data, err := json.Marshal(g)
	if err != nil {
		log.Printf("Warning: cannot encode game %s: %v", g.ID, err)
		return
	}
	_, err = s.db.Exec(`INSERT INTO games (id, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		g.ID, string(data), time.Now().Unix())
	if err != nil {
		log.Printf("Warning: cannot persist game %s: %v", g.ID, err)
	}
}

func (s *GameStore) remove(id string) {
	delete(s.games, id)
	if s.db != nil {
		if _, err := s.db.Exec(`DELETE FROM games WHERE id = ?`, id); err != nil {
			log.Printf("Warning: cannot delete game %s: %v", id, err)
		}
	}
}

func randomSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]) & 0x7fffffffffffffff)
}

// ─── Immich helpers (owner credentials) ────────────────────────────────────

var errGameNoOwnerCredentials = errors.New("the game owner has no usable Immich credentials")

// ownerAuth returns a function that attaches the game owner's Immich
// credentials to an upstream request. The stored account API key is
// preferred (works while the owner is offline); an SSO owner falls back to a
// live session.
func (s *Server) ownerAuth(serverURL, owner string) (func(*http.Request), error) {
	if s.accounts != nil {
		if acc, ok := s.accounts.Get(serverURL, owner); ok && acc.APIKey != "" {
			key := acc.APIKey
			return func(r *http.Request) {
				stripClientAuthHeaders(r)
				r.Header.Set("x-api-key", key)
			}, nil
		}
	}
	if s.session != nil {
		if _, sess, ok := s.session.Find(serverURL, owner); ok {
			cp := *sess
			return func(r *http.Request) { applySessionAuth(r, &cp) }, nil
		}
	}
	return nil, errGameNoOwnerCredentials
}

func (s *Server) gameImmichJSON(ctx context.Context, serverURL string, auth func(*http.Request), method, path string, body interface{}, out interface{}) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(serverURL, "/")+"/api"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	auth(req)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("immich %s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type immichAssetLite struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	OriginalFileName string `json:"originalFileName"`
	FileCreatedAt    string `json:"fileCreatedAt"`
	LocalDateTime    string `json:"localDateTime"`
	IsTrashed        bool   `json:"isTrashed"`
}

// gameFetchAssets builds the asset pool. Overridable in tests.
var gameFetchAssets = func(s *Server, ctx context.Context, serverURL string, auth func(*http.Request), albumID string, count int, imagesOnly bool) ([]immichAssetLite, error) {
	keep := func(a immichAssetLite) bool {
		if a.ID == "" || a.IsTrashed {
			return false
		}
		return !imagesOnly || a.Type == "IMAGE"
	}

	if albumID != "" {
		var album struct {
			Assets []immichAssetLite `json:"assets"`
		}
		if err := s.gameImmichJSON(ctx, serverURL, auth, http.MethodGet, "/albums/"+url.PathEscape(albumID), nil, &album); err != nil {
			return nil, err
		}
		out := make([]immichAssetLite, 0, len(album.Assets))
		for _, a := range album.Assets {
			if keep(a) {
				out = append(out, a)
			}
		}
		mrand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		if len(out) > count {
			out = out[:count]
		}
		return out, nil
	}

	seen := map[string]bool{}
	out := make([]immichAssetLite, 0, count)
	for attempt := 0; attempt < 10 && len(out) < count; attempt++ {
		body := map[string]interface{}{"size": min(250, count-len(out)+10)}
		if imagesOnly {
			body["type"] = "IMAGE"
		}
		var batch []immichAssetLite
		if err := s.gameImmichJSON(ctx, serverURL, auth, http.MethodPost, "/search/random", body, &batch); err != nil {
			return nil, err
		}
		added := 0
		for _, a := range batch {
			if keep(a) && !seen[a.ID] {
				seen[a.ID] = true
				out = append(out, a)
				added++
				if len(out) >= count {
					break
				}
			}
		}
		if added == 0 && len(batch) == 0 {
			break
		}
	}
	return out, nil
}

// gameTrash / gameRestore are overridable in tests.
var gameTrash = func(s *Server, ctx context.Context, serverURL string, auth func(*http.Request), id string) error {
	return s.gameImmichJSON(ctx, serverURL, auth, http.MethodDelete, "/assets", map[string]interface{}{"ids": []string{id}, "force": false}, nil)
}

var gameRestore = func(s *Server, ctx context.Context, serverURL string, auth func(*http.Request), id string) error {
	return s.gameImmichJSON(ctx, serverURL, auth, http.MethodPost, "/trash/restore/assets", map[string]interface{}{"ids": []string{id}}, nil)
}

// ─── HTTP ──────────────────────────────────────────────────────────────────

func gameError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// gameHandler routes everything under /api/game (behind authMiddleware).
func (s *Server) gameHandler(w http.ResponseWriter, r *http.Request) {
	session := sessionFromContext(r.Context())
	if session == nil {
		gameError(w, http.StatusUnauthorized, "no_session", "no session")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/game"), "/")
	parts := []string{}
	if rest != "" {
		parts = strings.Split(rest, "/")
	}

	switch {
	case len(parts) == 0 && r.Method == http.MethodGet:
		s.gameListHandler(w, r, session)
	case len(parts) == 0 && r.Method == http.MethodPost:
		s.gameCreateHandler(w, r, session)
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.gameStateHandler(w, r, session, parts[0])
	case len(parts) == 1 && r.Method == http.MethodDelete:
		s.gameDeleteHandler(w, r, session, parts[0])
	case len(parts) == 2 && parts[1] == "vote" && r.Method == http.MethodPost:
		s.gameVoteHandler(w, r, session, parts[0])
	case len(parts) == 2 && parts[1] == "undo" && r.Method == http.MethodPost:
		s.gameUndoHandler(w, r, session, parts[0])
	case len(parts) == 2 && parts[1] == "review" && r.Method == http.MethodGet:
		s.gameReviewHandler(w, r, session, parts[0])
	case len(parts) == 2 && parts[1] == "resolve" && r.Method == http.MethodPost:
		s.gameResolveHandler(w, r, session, parts[0])
	case len(parts) == 4 && parts[1] == "assets" && r.Method == http.MethodGet &&
		(parts[3] == "thumbnail" || parts[3] == "original"):
		s.gameMediaHandler(w, r, session, parts[0], parts[2], parts[3])
	default:
		gameError(w, http.StatusNotFound, "not_found", "not found")
	}
}

// lookup returns the game when the session's user is a player.
// Callers must hold s.games.mu.
func (s *Server) gameFor(session *Session, id string) (*Game, bool) {
	g, ok := s.games.games[id]
	if !ok || g.ServerURL != session.ServerURL || !g.hasPlayer(session.UserName) {
		return nil, false
	}
	return g, true
}

func gameSummary(g *Game, player string) map[string]interface{} {
	progress := make(map[string]int, len(g.Players))
	for _, p := range g.Players {
		progress[p] = g.votedCount(p)
	}
	return map[string]interface{}{
		"id":        g.ID,
		"name":      g.Name,
		"owner":     g.Owner,
		"players":   g.Players,
		"isOwner":   g.Owner == player,
		"total":     len(g.Assets),
		"progress":  progress,
		"counts":    g.counts(),
		"finished":  g.finished(),
		"createdAt": g.CreatedAt,
		"source":    g.Source,
	}
}

// publicAsset is the per-player view: other players' votes stay hidden until
// the asset is decided.
func publicAsset(g *Game, a *GameAsset, player string) map[string]interface{} {
	out := map[string]interface{}{
		"id":               a.ID,
		"type":             a.Type,
		"originalFileName": a.FileName,
		"fileCreatedAt":    a.FileCreatedAt,
		"localDateTime":    a.LocalDateTime,
		"status":           a.Status,
		"myVote":           a.Votes[player],
	}
	if a.Status != gameStatusPending {
		out["votes"] = a.Votes
		out["decidedBy"] = a.DecidedBy
	}
	return out
}

func (s *Server) gameListHandler(w http.ResponseWriter, _ *http.Request, session *Session) {
	s.games.mu.Lock()
	defer s.games.mu.Unlock()
	list := []map[string]interface{}{}
	for _, g := range s.games.games {
		if g.ServerURL == session.ServerURL && g.hasPlayer(session.UserName) {
			list = append(list, gameSummary(g, session.UserName))
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i]["createdAt"].(int64) > list[j]["createdAt"].(int64)
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"games": list})
}

func (s *Server) gameCreateHandler(w http.ResponseWriter, r *http.Request, session *Session) {
	var req struct {
		Name       string   `json:"name"`
		Partners   []string `json:"partners"`
		Partner    string   `json:"partner"`
		Count      int      `json:"count"`
		AlbumID    string   `json:"albumId"`
		ImagesOnly bool     `json:"imagesOnly"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		gameError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Swipe game " + time.Now().Format("Jan 2")
	}
	if len([]rune(name)) > gameMaxNameLen {
		name = string([]rune(name)[:gameMaxNameLen])
	}
	if req.Partner != "" {
		req.Partners = append(req.Partners, req.Partner)
	}
	players := []string{session.UserName}
	for _, p := range req.Partners {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		dup := false
		for _, existing := range players {
			if existing == p {
				dup = true
			}
		}
		if dup {
			continue
		}
		if !s.gameKnownUser(session.ServerURL, p) {
			gameError(w, http.StatusBadRequest, "unknown_partner",
				fmt.Sprintf("%q has no Swipe account on this Immich server", p))
			return
		}
		players = append(players, p)
	}
	if len(players) < 2 {
		gameError(w, http.StatusBadRequest, "partner_required", "invite at least one partner")
		return
	}
	if len(players) > gameMaxPlayers {
		gameError(w, http.StatusBadRequest, "too_many_players", fmt.Sprintf("at most %d players", gameMaxPlayers))
		return
	}
	count := req.Count
	if count <= 0 {
		count = gameDefaultAssets
	}
	if count > gameMaxAssets {
		count = gameMaxAssets
	}

	auth, err := s.ownerAuth(session.ServerURL, session.UserName)
	if err != nil {
		gameError(w, http.StatusPreconditionRequired, "api_key_required",
			"add your Immich API key in Settings before creating a game")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	albumID := strings.TrimSpace(req.AlbumID)
	fetched, err := gameFetchAssets(s, ctx, session.ServerURL, auth, albumID, count, req.ImagesOnly)
	if err != nil {
		log.Printf("game: fetching assets failed: %v", err)
		gameError(w, http.StatusBadGateway, "immich_error", "could not load photos from Immich")
		return
	}
	if len(fetched) == 0 {
		gameError(w, http.StatusBadRequest, "no_assets", "no photos found for this game")
		return
	}

	g := &Game{
		ID:        generateToken()[:16],
		Name:      name,
		ServerURL: session.ServerURL,
		Owner:     session.UserName,
		Players:   players,
		Seeds:     map[string]int64{},
		CreatedAt: time.Now().Unix(),
		Source:    "random",
	}
	if albumID != "" {
		g.Source = "album:" + albumID
	}
	for _, p := range players {
		g.Seeds[p] = randomSeed()
	}
	for _, a := range fetched {
		g.Assets = append(g.Assets, &GameAsset{
			ID: a.ID, Type: a.Type, FileName: a.OriginalFileName,
			FileCreatedAt: a.FileCreatedAt, LocalDateTime: a.LocalDateTime,
			Votes: map[string]string{}, Status: gameStatusPending,
		})
	}
	g.rebuild()

	s.games.mu.Lock()
	s.games.games[g.ID] = g
	s.games.save(g)
	summary := gameSummary(g, session.UserName)
	s.games.mu.Unlock()

	go s.gameNotifyInvite(g, players[1:])
	writeJSON(w, http.StatusCreated, summary)
}

// gameKnownUser reports whether the name is a Swipe account (or an active SSO
// session) on this Immich server.
func (s *Server) gameKnownUser(serverURL, userName string) bool {
	if s.accounts != nil {
		if _, ok := s.accounts.Get(serverURL, userName); ok {
			return true
		}
	}
	if s.session != nil {
		if _, _, ok := s.session.Find(serverURL, userName); ok {
			return true
		}
	}
	return false
}

func (s *Server) gameStateHandler(w http.ResponseWriter, r *http.Request, session *Session, id string) {
	limit := 5
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 20 {
		limit = v
	}
	s.games.mu.Lock()
	defer s.games.mu.Unlock()
	g, ok := s.gameFor(session, id)
	if !ok {
		gameError(w, http.StatusNotFound, "not_found", "game not found")
		return
	}
	next := []map[string]interface{}{}
	for _, a := range g.nextFor(session.UserName, limit) {
		next = append(next, publicAsset(g, a, session.UserName))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"game": gameSummary(g, session.UserName),
		"next": next,
	})
}

func (s *Server) gameDeleteHandler(w http.ResponseWriter, _ *http.Request, session *Session, id string) {
	s.games.mu.Lock()
	defer s.games.mu.Unlock()
	g, ok := s.gameFor(session, id)
	if !ok {
		gameError(w, http.StatusNotFound, "not_found", "game not found")
		return
	}
	if g.Owner != session.UserName {
		gameError(w, http.StatusForbidden, "owner_only", "only the game owner can end a game")
		return
	}
	s.games.remove(id)
	w.WriteHeader(http.StatusNoContent)
}

type gameAssetRequest struct {
	AssetID  string `json:"assetId"`
	Vote     string `json:"vote"`
	Decision string `json:"decision"`
}

func decodeGameAssetRequest(r *http.Request) (gameAssetRequest, error) {
	var req gameAssetRequest
	err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
	return req, err
}

func (s *Server) gameVoteHandler(w http.ResponseWriter, r *http.Request, session *Session, id string) {
	req, err := decodeGameAssetRequest(r)
	if err != nil || (req.Vote != gameVoteKeep && req.Vote != gameVoteDelete) {
		gameError(w, http.StatusBadRequest, "invalid_body", `vote must be "keep" or "delete"`)
		return
	}
	player := session.UserName

	s.games.mu.Lock()
	g, ok := s.gameFor(session, id)
	if !ok {
		s.games.mu.Unlock()
		gameError(w, http.StatusNotFound, "not_found", "game not found")
		return
	}
	a, ok := g.asset(req.AssetID)
	if !ok {
		s.games.mu.Unlock()
		gameError(w, http.StatusNotFound, "asset_not_found", "photo is not part of this game")
		return
	}
	if a.Status != gameStatusPending {
		s.games.mu.Unlock()
		gameError(w, http.StatusConflict, "already_decided", "this photo is already decided")
		return
	}
	a.Votes[player] = req.Vote
	outcome := "waiting"
	needTrash := false
	if g.allVoted(a) {
		switch g.tally(a) {
		case gameVoteKeep:
			a.Status, outcome = gameStatusKept, "kept"
		case gameVoteDelete:
			// Mark as decided now so a concurrent vote cannot double-resolve;
			// the trash call happens outside the lock.
			a.Status, outcome, needTrash = gameStatusDeleted, "deleted", true
		default:
			a.Status, outcome = gameStatusDisputed, "disputed"
		}
		a.DecidedBy, a.DecidedAt = "agreement", time.Now().Unix()
	}
	s.games.save(g)
	serverURL, owner := g.ServerURL, g.Owner
	s.games.mu.Unlock()

	if needTrash {
		if err := s.gameApplyDelete(r.Context(), serverURL, owner, g, a); err != nil {
			outcome = "error"
		}
	}

	s.games.mu.Lock()
	summary := gameSummary(g, player)
	asset := publicAsset(g, a, player)
	s.gameMaybeNotifyEnd(r, g)
	s.games.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"outcome": outcome,
		"asset":   asset,
		"game":    summary,
	})
}

// gameApplyDelete trashes the asset with the owner's credentials and records
// a failure on the asset (status error) so the owner can retry from review.
func (s *Server) gameApplyDelete(ctx context.Context, serverURL, owner string, g *Game, a *GameAsset) error {
	auth, err := s.ownerAuth(serverURL, owner)
	if err == nil {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err = gameTrash(s, tctx, serverURL, auth, a.ID)
		cancel()
	}
	s.games.mu.Lock()
	defer s.games.mu.Unlock()
	if err != nil {
		log.Printf("game %s: trashing asset %s failed: %v", g.ID, a.ID, err)
		a.Status = gameStatusError
		a.Error = "moving to trash failed; retry from the review screen"
	} else {
		a.Status = gameStatusDeleted
		a.Error = ""
	}
	s.games.save(g)
	return err
}

func (s *Server) gameUndoHandler(w http.ResponseWriter, r *http.Request, session *Session, id string) {
	req, err := decodeGameAssetRequest(r)
	if err != nil || req.AssetID == "" {
		gameError(w, http.StatusBadRequest, "invalid_body", "assetId required")
		return
	}
	player := session.UserName

	s.games.mu.Lock()
	g, ok := s.gameFor(session, id)
	if !ok {
		s.games.mu.Unlock()
		gameError(w, http.StatusNotFound, "not_found", "game not found")
		return
	}
	a, ok := g.asset(req.AssetID)
	if !ok {
		s.games.mu.Unlock()
		gameError(w, http.StatusNotFound, "asset_not_found", "photo is not part of this game")
		return
	}
	if _, voted := a.Votes[player]; !voted {
		s.games.mu.Unlock()
		gameError(w, http.StatusConflict, "no_vote", "you have not voted on this photo")
		return
	}
	if a.DecidedBy == "admin" {
		s.games.mu.Unlock()
		gameError(w, http.StatusConflict, "admin_decided", "the game owner already decided this photo")
		return
	}
	wasDeleted := a.Status == gameStatusDeleted
	serverURL, owner := g.ServerURL, g.Owner
	s.games.mu.Unlock()

	if wasDeleted {
		auth, err := s.ownerAuth(serverURL, owner)
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			err = gameRestore(s, ctx, serverURL, auth, a.ID)
			cancel()
		}
		if err != nil {
			log.Printf("game %s: restoring asset %s failed: %v", g.ID, a.ID, err)
			gameError(w, http.StatusBadGateway, "restore_failed", "could not restore the photo from the Immich trash")
			return
		}
	}

	s.games.mu.Lock()
	delete(a.Votes, player)
	a.Status, a.DecidedBy, a.DecidedAt, a.Error = gameStatusPending, "", 0, ""
	g.NotifiedEnd = false
	s.games.save(g)
	resp := map[string]interface{}{
		"asset": publicAsset(g, a, player),
		"game":  gameSummary(g, player),
	}
	s.games.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) gameReviewHandler(w http.ResponseWriter, _ *http.Request, session *Session, id string) {
	s.games.mu.Lock()
	defer s.games.mu.Unlock()
	g, ok := s.gameFor(session, id)
	if !ok {
		gameError(w, http.StatusNotFound, "not_found", "game not found")
		return
	}
	if g.Owner != session.UserName {
		gameError(w, http.StatusForbidden, "owner_only", "only the game owner reviews disputes")
		return
	}
	items := []map[string]interface{}{}
	for _, a := range g.Assets {
		if a.Status == gameStatusDisputed || a.Status == gameStatusError {
			items = append(items, map[string]interface{}{
				"id":               a.ID,
				"type":             a.Type,
				"originalFileName": a.FileName,
				"fileCreatedAt":    a.FileCreatedAt,
				"localDateTime":    a.LocalDateTime,
				"status":           a.Status,
				"votes":            a.Votes,
				"error":            a.Error,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"game":  gameSummary(g, session.UserName),
		"items": items,
	})
}

func (s *Server) gameResolveHandler(w http.ResponseWriter, r *http.Request, session *Session, id string) {
	req, err := decodeGameAssetRequest(r)
	if err != nil || (req.Decision != gameVoteKeep && req.Decision != gameVoteDelete) {
		gameError(w, http.StatusBadRequest, "invalid_body", `decision must be "keep" or "delete"`)
		return
	}
	s.games.mu.Lock()
	g, ok := s.gameFor(session, id)
	if !ok {
		s.games.mu.Unlock()
		gameError(w, http.StatusNotFound, "not_found", "game not found")
		return
	}
	if g.Owner != session.UserName {
		s.games.mu.Unlock()
		gameError(w, http.StatusForbidden, "owner_only", "only the game owner resolves disputes")
		return
	}
	a, ok := g.asset(req.AssetID)
	if !ok {
		s.games.mu.Unlock()
		gameError(w, http.StatusNotFound, "asset_not_found", "photo is not part of this game")
		return
	}
	if a.Status != gameStatusDisputed && a.Status != gameStatusError && a.Status != gameStatusPending {
		s.games.mu.Unlock()
		gameError(w, http.StatusConflict, "already_decided", "this photo is already decided")
		return
	}
	a.DecidedBy, a.DecidedAt = "admin", time.Now().Unix()
	needTrash := false
	if req.Decision == gameVoteKeep {
		a.Status, a.Error = gameStatusKept, ""
	} else {
		a.Status, needTrash = gameStatusDeleted, true
	}
	s.games.save(g)
	serverURL, owner := g.ServerURL, g.Owner
	s.games.mu.Unlock()

	status := http.StatusOK
	if needTrash {
		if err := s.gameApplyDelete(r.Context(), serverURL, owner, g, a); err != nil {
			status = http.StatusBadGateway
		}
	}
	s.games.mu.Lock()
	resp := map[string]interface{}{"status": a.Status, "game": gameSummary(g, session.UserName)}
	s.games.mu.Unlock()
	writeJSON(w, status, resp)
}

// gameMediaHandler streams a game asset's thumbnail/original using the
// owner's credentials, so partners need no access to the owner's library.
func (s *Server) gameMediaHandler(w http.ResponseWriter, r *http.Request, session *Session, id, assetID, kind string) {
	s.games.mu.Lock()
	g, ok := s.gameFor(session, id)
	var inGame bool
	var serverURL, owner string
	if ok {
		_, inGame = g.asset(assetID)
		serverURL, owner = g.ServerURL, g.Owner
	}
	s.games.mu.Unlock()
	if !ok || !inGame {
		gameError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	auth, err := s.ownerAuth(serverURL, owner)
	if err != nil {
		gameError(w, http.StatusServiceUnavailable, "owner_offline", "the game owner's Immich access is unavailable")
		return
	}
	target, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil {
		gameError(w, http.StatusBadRequest, "invalid_server", "invalid server URL")
		return
	}
	query := url.Values{}
	if kind == "thumbnail" {
		size := r.URL.Query().Get("size")
		if size != "thumbnail" && size != "fullsize" {
			size = "preview"
		}
		query.Set("size", size)
	}
	upstreamPath := "/api/assets/" + url.PathEscape(assetID) + "/" + kind
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.URL.Path = strings.TrimRight(target.Path, "/") + upstreamPath
			req.URL.RawPath = ""
			req.URL.RawQuery = query.Encode()
			req.Header.Del("Cookie")
			auth(req)
		},
		Transport: s.transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("game media proxy error: %v", err)
			gameError(w, http.StatusBadGateway, "upstream_failed", "upstream request failed")
		},
	}
	proxy.ServeHTTP(w, r)
}

// ─── Email notifications ──────────────────────────────────────────────────

func (s *Server) gameAppURL(r *http.Request, path string) string {
	base := ""
	if s.config.PublicURL != "" {
		base = strings.TrimRight(s.config.PublicURL, "/")
	} else if r != nil {
		base = s.oauthPublicBase(r)
	}
	return base + path
}

func (s *Server) gameEmailFor(serverURL, userName string) string {
	if s.accounts == nil {
		return ""
	}
	if acc, ok := s.accounts.Get(serverURL, userName); ok && acc.Email != nil {
		return strings.TrimSpace(*acc.Email)
	}
	return ""
}

func (s *Server) gameSend(to, subject, body string) {
	if to == "" {
		return
	}
	es, ok := s.effectiveEmailSettings()
	if !ok {
		return
	}
	if err := sendMailFunc(es, to, subject, body); err != nil {
		log.Printf("game: email to %s failed: %v", to, err)
	}
}

func (s *Server) gameNotifyInvite(g *Game, invitees []string) {
	for _, p := range invitees {
		to := s.gameEmailFor(g.ServerURL, p)
		s.gameSend(to, fmt.Sprintf("%s invited you to a Swipe game", g.Owner), fmt.Sprintf(
			"%s started the Swipe game %q with %d photos.\n\n"+
				"Swipe right to keep, left to delete. A photo is only deleted when everyone agrees; "+
				"disagreements go to %s to decide.\n\nOpen Immich Swipe and tap the game icon to play.\n",
			g.Owner, g.Name, len(g.Assets), g.Owner))
	}
}

// gameMaybeNotifyEnd emails the owner once every photo has been voted on by
// everyone. Callers must hold s.games.mu.
func (s *Server) gameMaybeNotifyEnd(r *http.Request, g *Game) {
	if g.NotifiedEnd || !g.finished() {
		return
	}
	g.NotifiedEnd = true
	s.games.save(g)
	c := g.counts()
	link := s.gameAppURL(r, "/game/"+g.ID+"/review")
	to := s.gameEmailFor(g.ServerURL, g.Owner)
	subject := fmt.Sprintf("Swipe game %q finished", g.Name)
	body := fmt.Sprintf("Everyone finished the Swipe game %q.\n\nKept: %d\nDeleted (moved to Immich trash): %d\nDisputed, waiting for you: %d\nFailed deletes to retry: %d\n",
		g.Name, c[gameStatusKept], c[gameStatusDeleted], c[gameStatusDisputed], c[gameStatusError])
	if c[gameStatusDisputed]+c[gameStatusError] > 0 {
		body += "\nReview them here: " + link + "\n"
	}
	go s.gameSend(to, subject, body)
}
