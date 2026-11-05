package cloudfiles_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/luskaner/ageServer/common/game"
	"github.com/luskaner/ageServer/server/internal"
	"github.com/luskaner/ageServer/server/internal/models"
	"github.com/luskaner/ageServer/server/internal/routes/cloudfiles"
	"github.com/luskaner/ageServer/server/internal/routes/game/cloud"
	"github.com/luskaner/ageServer/server/internal/routes/game/leaderboard"
	"github.com/luskaner/ageServer/server/internal/routes/game/party"
)

type stubResources struct {
	models.Resources
	cf models.CloudFiles
}

func (s *stubResources) CloudFiles() models.CloudFiles { return s.cf }

type stubGame struct {
	models.Game
	title    string
	res      models.Resources
	users    models.Users
	sessions models.Sessions
	history  models.MatchHistory
	replays  models.MatchReplays
}

func (g *stubGame) Title() string                         { return g.title }
func (g *stubGame) Resources() models.Resources           { return g.res }
func (g *stubGame) Users() models.Users                   { return g.users }
func (g *stubGame) Sessions() models.Sessions             { return g.sessions }
func (g *stubGame) MatchHistory() models.MatchHistory     { return g.history }
func (g *stubGame) MatchReplays() models.MatchReplays     { return g.replays }
func (g *stubGame) Advertisements() models.Advertisements { return nil }

// The match captured from AoE IV on 2026-11-05.
const (
	testMatchID = 908104717
	playerA     = 206197877
	playerB     = 419608597
	apiHost     = "aoe-api.worldsedgelink.com"
)

type replayEnv struct {
	t       *testing.T
	game    *stubGame
	folder  string
	store   *models.MainMatchReplays
	history *models.MatchHistoryStore
	session map[int32]models.Session
}

func newReplayEnv(t *testing.T, enabled bool, maxFileSize int64) *replayEnv {
	t.Helper()
	internal.InitializeRng(42)
	folder := t.TempDir()
	store := &models.MainMatchReplays{}
	store.Initialize(game.AoE4, &models.MatchReplaysOpts{Enabled: enabled, Folder: folder, MaxFileSize: maxFileSize, MaxTotalSize: 64 << 20})
	t.Cleanup(store.Close)
	history := models.NewMatchHistoryStore(t.TempDir())
	t.Cleanup(func() { _ = history.Close() })
	users := &models.MainUsers{}
	users.Initialize()
	sessions := &models.MainSessions{}
	sessions.Initialize()
	env := &replayEnv{
		t:      t,
		folder: folder,
		store:  store,
		game: &stubGame{
			title:    game.AoE4,
			res:      &stubResources{cf: models.CloudFiles{Value: map[string]models.CloudfilesIndex{}, Credentials: models.NewCredentials()}},
			users:    users,
			sessions: sessions,
			history:  history,
			replays:  store,
		},
		history: history,
		session: map[int32]models.Session{},
	}
	for _, id := range []int32{playerA, playerB, 1} {
		sess, _ := sessions.GetById(sessions.Create(id, 1))
		env.session[id] = sess
	}
	return env
}

// hostLobby records the lobby as host.go / update.go do.
func (e *replayEnv) hostLobby(matchID int64) {
	e.history.RecordPendingMatch(models.MatchResult{
		MatchID: matchID, MapName: "dry_arabia", MaxPlayers: 2, EncodedOptions: "eNqtUtuO", EncodedSlots: "eNrtm11v",
		Description: "unnamed_session", DataCRC: -458542202, AppBinaryCRC: 12182,
	})
}

func (e *replayEnv) request(method, target string, body io.Reader, as int32) *http.Request {
	req := httptest.NewRequest(method, target, body)
	ctx := context.WithValue(req.Context(), "game", models.Game(e.game))
	if as != 0 {
		ctx = context.WithValue(ctx, "session", e.session[as])
	}
	return req.WithContext(ctx)
}

func (e *replayEnv) call(h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func makeGzippedB64(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// reportMatch posts a final result (results [1,0]) as player `as`.
func (e *replayEnv) reportMatch(matchID int64, as int32, results string) []any {
	e.t.Helper()
	form := url.Values{
		"match_id":    {strconv.FormatInt(matchID, 10)},
		"profile_ids": {"[206197877,419608597]"},
		"results":     {results},
		"teamIDs":     {"[0,1]"},
		"race_ids":    {"[2121952,2121948]"},
		"xpGained":    {"[400,200]"},
		"countersZip": {`"` + makeGzippedB64(e.t, []string{`{"ekills":42}`, `{"ekills":15}`}) + `"`},
		"itemUpdates": {`["{\"clientUpdates\":{\"winReason\":\"Conquest\"}}","{\"clientUpdates\":{\"winReason\":\"Conquest\"}}"]`},
	}
	req := e.request(http.MethodPost, "https://"+apiHost+"/game/party/reportMatch", strings.NewReader(form.Encode()), as)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp []any
	if err := json.Unmarshal(e.call(party.ReportMatch, req).Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("reportMatch response: %v", err)
	}
	return resp
}

type uploadTarget struct {
	host, path, sas string
}

func target(t *testing.T, slot any) (uploadTarget, bool) {
	t.Helper()
	arr := slot.([]any)
	if arr[0].(float64) == -1 {
		return uploadTarget{}, false
	}
	return uploadTarget{arr[1].(string), arr[2].(string), arr[3].(string)}, true
}

func (e *replayEnv) cloudfile(method, path, sas string, body []byte, headers ...string) *httptest.ResponseRecorder {
	target := "https://" + apiHost + path
	if sas != "" {
		target += "?" + sas
	}
	req := e.request(method, target, bytes.NewReader(body), 0)
	for k := 0; k+1 < len(headers); k += 2 {
		req.Header.Set(headers[k], headers[k+1])
	}
	return e.call(cloudfiles.Cloudfiles, req)
}

func (e *replayEnv) tempCredentials(key string, as int32) []any {
	e.t.Helper()
	req := e.request(http.MethodGet, "https://"+apiHost+"/game/cloud/getTempCredentials?key="+url.QueryEscape(key), nil, as)
	var resp []any
	if err := json.Unmarshal(e.call(cloud.GetTempCredentials, req).Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func TestMatchReplayStoredLocallyAndListed(t *testing.T) {
	env := newReplayEnv(t, true, 1<<20)
	env.hostLobby(testMatchID)

	resp := env.reportMatch(testMatchID, playerA, "[1,0]")
	if resp[0].(float64) != 0 {
		t.Fatalf("no upload offered: %v", resp)
	}
	replay, ok := target(t, resp[2])
	if !ok {
		t.Fatalf("no replay target: %v", resp[2])
	}
	// The upload goes to this server (the game API host it already talks to),
	// never to the official storage.
	if replay.host != apiHost || replay.path != "/cloudfiles/replays/908104717/replay.rec.gz" || !strings.Contains(replay.sas, "sp=rcw") {
		t.Fatalf("replay target %+v", replay)
	}
	if _, ok := target(t, resp[3]); !ok {
		t.Fatalf("no stats target: %v", resp[3])
	}

	payload := []byte("fake-gzipped-aoe4-replay-stream-payload")
	if rr := env.cloudfile(http.MethodPut, replay.path, replay.sas, payload); rr.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	onDisk, err := os.ReadFile(filepath.Join(env.folder, game.AoE4, "908104717-replay.rec.gz"))
	if err != nil || !bytes.Equal(onDisk, payload) {
		t.Fatalf("replay not on the local disk: %q (%v)", onDisk, err)
	}

	// Match history: metadata kept and the replay offered.
	req := env.request(http.MethodGet, "https://"+apiHost+"/game/Leaderboard/getRecentMatchHistory?profile_ids=%5B-1%5D", nil, playerA)
	var hist []any
	if err := json.Unmarshal(env.call(leaderboard.GetRecentMatchHistory, req).Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	match := hist[1].([]any)[0].([]any)
	if match[2].(string) != "dry_arabia" || match[5].(string) != "eNqtUtuO" || match[6].(string) != "eNrtm11v" {
		t.Fatalf("lobby metadata lost: %v", match[:8])
	}
	urls := match[13].([]any)
	if len(urls) != 1 {
		t.Fatalf("matchUrls %v", urls)
	}
	entry := urls[0].([]any)
	if entry[1].(string) != replay.path || int(entry[2].(float64)) != len(payload) {
		t.Fatalf("matchUrl %v", entry)
	}
	version := match[14].([]any)
	if int32(version[4].(float64)) != -458542202 || int32(version[5].(float64)) != 12182 {
		t.Fatalf("version info %v", version)
	}

	// Download with a read token.
	cred := env.tempCredentials(replay.path, playerB)
	if cred[0].(float64) != 0 || !strings.Contains(cred[2].(string), "sp=r&") {
		t.Fatalf("getTempCredentials %v", cred)
	}
	rr := env.cloudfile(http.MethodGet, replay.path, cred[2].(string), nil)
	sum := md5.Sum(payload)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), payload) || rr.Header().Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatalf("GET: %d %q md5=%q", rr.Code, rr.Body.String(), rr.Header().Get("Content-MD5"))
	}
	rr = env.cloudfile(http.MethodGet, replay.path, cred[2].(string), nil, "Range", "bytes=0-3")
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "fake" || rr.Header().Get("Content-MD5") != "" {
		t.Fatalf("range GET: %d %q md5=%q", rr.Code, rr.Body.String(), rr.Header().Get("Content-MD5"))
	}

	// getFileURL points back at this server.
	req = env.request(http.MethodGet, "https://"+apiHost+"/game/cloud/getFileURL?names="+url.QueryEscape(`["`+replay.path+`"]`), nil, playerB)
	var fileURL []any
	if err := json.Unmarshal(env.call(cloud.GetFileURL, req).Body.Bytes(), &fileURL); err != nil {
		t.Fatal(err)
	}
	desc := fileURL[1].([]any)[0].([]any)
	if fileURL[0].(float64) != 0 || desc[3].(string) != "https://"+apiHost+replay.path || int(desc[1].(float64)) != len(payload) {
		t.Fatalf("getFileURL %v", fileURL)
	}

	// Once stored, a later report no longer asks for that blob.
	again := env.reportMatch(testMatchID, playerB, "[1,0]")
	if _, ok := target(t, again[2]); ok {
		t.Fatalf("replay requested again: %v", again[2])
	}
}

func TestMatchReplayRequiresTheRightToken(t *testing.T) {
	env := newReplayEnv(t, true, 1<<20)
	env.hostLobby(testMatchID)
	replay, _ := target(t, env.reportMatch(testMatchID, playerA, "[1,0]")[2])
	if rr := env.cloudfile(http.MethodPut, replay.path, replay.sas, []byte("original")); rr.Code != http.StatusCreated {
		t.Fatalf("PUT: %d", rr.Code)
	}
	read := env.tempCredentials(replay.path, playerB)[2].(string)
	other, _ := target(t, env.reportMatch(testMatchID, playerA, "[1,0]")[3]) // stats token

	for _, tc := range []struct {
		name, method, sas string
	}{
		{"PUT without token", http.MethodPut, ""},
		{"GET without token", http.MethodGet, ""},
		{"PUT with forged token", http.MethodPut, "sig=forged"},
		{"PUT with a read token", http.MethodPut, read},
		{"PUT with another blob's token", http.MethodPut, other.sas},
	} {
		if rr := env.cloudfile(tc.method, replay.path, tc.sas, []byte("overwritten")); rr.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", tc.name, rr.Code)
		}
	}
	onDisk, _ := os.ReadFile(filepath.Join(env.folder, game.AoE4, "908104717-replay.rec.gz"))
	if string(onDisk) != "original" {
		t.Fatalf("replay changed to %q", onDisk)
	}
}

func TestNoUploadOfferedWhenItShouldNot(t *testing.T) {
	t.Run("lobby not hosted here", func(t *testing.T) {
		env := newReplayEnv(t, true, 1<<20)
		if resp := env.reportMatch(testMatchID, playerA, "[1,0]"); resp[0].(float64) != 2 {
			t.Fatalf("offered for an unknown match: %v", resp)
		}
	})
	t.Run("reporter is not a player", func(t *testing.T) {
		env := newReplayEnv(t, true, 1<<20)
		env.hostLobby(testMatchID)
		if resp := env.reportMatch(testMatchID, 1, "[1,0]"); resp[0].(float64) != 2 {
			t.Fatalf("offered to a non-player: %v", resp)
		}
	})
	t.Run("partial report", func(t *testing.T) {
		env := newReplayEnv(t, true, 1<<20)
		env.hostLobby(testMatchID)
		if resp := env.reportMatch(testMatchID, playerA, "[3,3]"); resp[0].(float64) != 2 {
			t.Fatalf("offered on a partial report: %v", resp)
		}
	})
	t.Run("feature disabled", func(t *testing.T) {
		env := newReplayEnv(t, false, 1<<20)
		env.hostLobby(testMatchID)
		if resp := env.reportMatch(testMatchID, playerA, "[1,0]"); resp[0].(float64) != 2 {
			t.Fatalf("offered while disabled: %v", resp)
		}
		if rr := env.cloudfile(http.MethodPut, "/cloudfiles/replays/908104717/replay.rec.gz", "", []byte("x")); rr.Code != http.StatusNotFound {
			t.Fatalf("PUT while disabled: %d", rr.Code)
		}
		if entries, _ := os.ReadDir(env.folder); len(entries) != 0 {
			t.Fatalf("disabled store created files: %v", entries)
		}
	})
}

func TestMatchReplayUploadLimits(t *testing.T) {
	env := newReplayEnv(t, true, 16)
	env.hostLobby(testMatchID)
	replay, _ := target(t, env.reportMatch(testMatchID, playerA, "[1,0]")[2])

	if rr := env.cloudfile(http.MethodPut, replay.path, replay.sas, bytes.Repeat([]byte("x"), 17)); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized PUT: %d", rr.Code)
	}
	if rr := env.cloudfile(http.MethodPut, replay.path, replay.sas+"&comp=block&blockid=AAAA", []byte("block")); rr.Code != http.StatusNotImplemented {
		t.Fatalf("block PUT: %d", rr.Code)
	}
	if entries, _ := os.ReadDir(filepath.Join(env.folder, game.AoE4)); len(entries) != 0 {
		t.Fatalf("rejected uploads left files: %v", entries)
	}
}

func TestReplayKeysOutsideTheIssuedOnesAreNotFound(t *testing.T) {
	env := newReplayEnv(t, true, 1<<20)
	if err := os.WriteFile(filepath.Join(env.folder, game.AoE4, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"replays/../secret.txt", "replays/908104717/other.bin", "replays/908104717/../../secret.txt"} {
		if resp := env.tempCredentials(key, playerA); resp[0].(float64) != 2 {
			t.Fatalf("getTempCredentials(%q) = %v", key, resp)
		}
	}
}

func TestGetTempUploadCredentialsOnlyForPlayers(t *testing.T) {
	env := newReplayEnv(t, true, 1<<20)
	env.hostLobby(testMatchID)
	env.reportMatch(testMatchID, playerA, "[1,0]")

	ask := func(key string, as int32) []any {
		req := env.request(http.MethodGet, "https://"+apiHost+"/game/cloud/getTempUploadCredentials?key="+url.QueryEscape(key), nil, as)
		var resp []any
		if err := json.Unmarshal(env.call(cloud.GetTempUploadCredentials, req).Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := ask("/cloudfiles/replays/908104717/replay.rec.gz", playerB); resp[0].(float64) != 0 || resp[1].(string) != apiHost || !strings.Contains(resp[3].(string), "sp=rcw") {
		t.Fatalf("player: %v", resp)
	}
	for _, tc := range []struct {
		key string
		as  int32
	}{
		{"/cloudfiles/replays/908104717/replay.rec.gz", 1},  // not a player
		{"/cloudfiles/replays/908104717/replay.rec.gz", 0},  // no session
		{"/cloudfiles/replays/123/replay.rec.gz", playerA},  // another match
		{"/cloudfiles/replays/908104717/evil.exe", playerA}, // not a replay blob
	} {
		if resp := ask(tc.key, tc.as); resp[0].(float64) != 2 {
			t.Fatalf("%q as %d: %v", tc.key, tc.as, resp)
		}
	}
}

func TestStaticCloudfilesRejectWrites(t *testing.T) {
	env := newReplayEnv(t, true, 1<<20)
	if rr := env.cloudfile(http.MethodPut, "/cloudfiles/Attrib.sga", "", []byte("x")); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on a static cloud file: %d", rr.Code)
	}
}
