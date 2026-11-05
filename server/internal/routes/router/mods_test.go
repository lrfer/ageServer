package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/luskaner/ageServer/common/game"
	"github.com/luskaner/ageServer/server/internal/routes/apiAgeOfEmpires/mods"
)

// withModsFolder points the mods store at a temp folder holding the given
// AoE IV archives (name -> content).
func withModsFolder(t *testing.T, archives map[string]string) {
	t.Helper()
	mods.ResetDefaultStore()
	orig := mods.ModsFolder
	mods.ModsFolder = t.TempDir()
	t.Cleanup(func() {
		mods.ModsFolder = orig
		mods.ResetDefaultStore()
	})
	dir := filepath.Join(mods.ModsFolder, game.AoE4)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range archives {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func serve(h http.Handler, method, host, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Steam1.0 TICKET")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeList(t *testing.T, rr *httptest.ResponseRecorder) mods.ModListResponse {
	t.Helper()
	var resp mods.ModListResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &resp) != nil {
		t.Fatalf("not a mod list: %d %s", rr.Code, rr.Body.String())
	}
	return resp
}

// The three requests AoE IV sends when the mod browser opens (captured 2026-11-04).
var capturedModQueries = map[string]string{
	"/api/v4/Mods/My/":        `{"q":"","filter":0,"sort":"","order":"","start":1,"count":20000,"game":4,"modid":""}`,
	"/api/v4/Mods/Installed/": `{"q":"","filter":0,"sort":"","order":"","start":0,"count":0,"game":4,"modid":""}`,
	"/api/v4/Mods/Find/":      `{"q":"","filter":0,"sort":"popular","order":"desc","start":0,"count":60,"game":4,"modid":""}`,
}

// Body the official service returned to a client it did not authenticate.
const officialUnauthorized = `{"searchParams":null,"modList":[],"totalCount":null,"isLoggedOn":false,"errorCode":900,"errorMessage":"Access Denied - User not authenticated."}`

// fakeOfficial is a stand-in for the official mod service.
type fakeOfficial struct {
	mu     sync.Mutex
	status int
	body   string
	paths  []string
	bodies []string
}

func (f *fakeOfficial) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.bodies = append(f.bodies, string(body))
	status, respBody := f.status, f.body
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, respBody)
}

func (f *fakeOfficial) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

// modsBehindOfficial returns the AoE IV mod routes with upstream pointing at
// official (or at a closed server when official is nil).
func modsBehindOfficial(t *testing.T, official *fakeOfficial) http.Handler {
	t.Helper()
	var srv *httptest.Server
	if official != nil {
		srv = httptest.NewServer(official)
		t.Cleanup(srv.Close)
	} else {
		srv = httptest.NewServer(http.NotFoundHandler())
		srv.Close() // unreachable
	}
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	group := &Group{mux: http.NewServeMux()}
	registerLocalModsRoutes(group, httputil.NewSingleHostReverseProxy(target))
	return MaxBodySizeMiddleware(1<<20, group.mux)
}

func TestLocalModsServedOfflineOnAoe4Host(t *testing.T) {
	withCanUseInternet(t, false)
	withModsFolder(t, nil)
	h := MaxBodySizeMiddleware(1<<20, NewAoe4ApiAgeOfEmpires().InitializeRoutes(game.AoE4, http.NotFoundHandler()))

	for path, body := range capturedModQueries {
		resp := decodeList(t, serve(h, "POST", "api-dr.ageofempires.com", path, []byte(body)))
		if resp.ErrorCode != 0 || !resp.IsLoggedOn || resp.ModList == nil {
			t.Fatalf("%s: %+v", path, resp)
		}
	}
	// An unknown mod is answered by the mods handler (JSON), not by the mux.
	rr := serve(h, "GET", "api-dr.ageofempires.com", "/api/v4/Mods/Detail/1", nil)
	if rr.Code != http.StatusNotFound || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Detail/1: %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
}

func TestLocalModsServedWhenOfficialHostDoesNotResolve(t *testing.T) {
	withCanUseInternet(t, true)
	withStubResolver(t, &stubResolver{dnsErr: errors.New("offline")})
	withModsFolder(t, map[string]string{"a.sga": "a"})
	h := NewAoe4ApiAgeOfEmpires().InitializeRoutes(game.AoE4, http.NotFoundHandler())

	resp := decodeList(t, serve(h, "POST", "api-dr.ageofempires.com", "/api/v4/Mods/Find/", []byte(capturedModQueries["/api/v4/Mods/Find/"])))
	if len(resp.ModList) != 1 {
		t.Fatalf("listed %d mods, want the local one", len(resp.ModList))
	}
}

func TestLocalModsWhenOfficialRejectsTheClient(t *testing.T) {
	withModsFolder(t, map[string]string{"a.sga": "local"})
	official := &fakeOfficial{status: http.StatusUnauthorized, body: officialUnauthorized}
	h := modsBehindOfficial(t, official)

	for path, body := range capturedModQueries {
		resp := decodeList(t, serve(h, "POST", "api-dr.ageofempires.com", path, []byte(body)))
		if resp.ErrorCode != 0 || !resp.IsLoggedOn {
			t.Fatalf("%s: official 401 leaked to the client: %+v", path, resp)
		}
	}
	if official.hits() != len(capturedModQueries) {
		t.Fatalf("official service got %d requests, want %d", official.hits(), len(capturedModQueries))
	}
	for k, path := range official.paths {
		if official.bodies[k] != capturedModQueries[path] {
			t.Fatalf("%s forwarded body %q, want the game's", path, official.bodies[k])
		}
	}

	// Path values and the body survive the replay to the local handler.
	find := decodeList(t, serve(h, "POST", "api-dr.ageofempires.com", "/api/v4/Mods/Find/", []byte(`{"game":4}`)))
	if len(find.ModList) != 1 {
		t.Fatalf("Find listed %d mods", len(find.ModList))
	}
	id := strconv.FormatInt(find.ModList[0].ModId, 10)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v4/Mods/Detail/" + id, ""},
		{"POST", "/api/v4/Mods/Detail/", `{"modid":"` + id + `"}`},
	} {
		rr := serve(h, tc.method, "api-dr.ageofempires.com", tc.path, []byte(tc.body))
		var item mods.ModItem
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &item) != nil || strconv.FormatInt(item.ModId, 10) != id {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestOfficialAnswerRelayedWhenItAcceptsTheClient(t *testing.T) {
	withModsFolder(t, map[string]string{"a.sga": "local"})
	officialBody := `{"searchParams":null,"modList":[{"modId":93963,"modName":"Online mod"}],"totalCount":1,"isLoggedOn":true,"errorCode":0,"errorMessage":null}`
	official := &fakeOfficial{status: http.StatusOK, body: officialBody}
	h := modsBehindOfficial(t, official)

	rr := serve(h, "POST", "api-dr.ageofempires.com", "/api/v4/Mods/Find/", []byte(capturedModQueries["/api/v4/Mods/Find/"]))
	if rr.Code != http.StatusOK || rr.Body.String() != officialBody {
		t.Fatalf("official answer not relayed: %d %s", rr.Code, rr.Body.String())
	}

	// Any other official status (here 404) is relayed too; only 401 falls back.
	official.mu.Lock()
	official.status, official.body = http.StatusNotFound, `{"errorCode":404}`
	official.mu.Unlock()
	if rr := serve(h, "GET", "api-dr.ageofempires.com", "/api/v4/Mods/Detail/93963", nil); rr.Code != http.StatusNotFound || rr.Body.String() != `{"errorCode":404}` {
		t.Fatalf("official 404 not relayed: %d %s", rr.Code, rr.Body.String())
	}
}

func TestLocalModsWhenOfficialUnreachable(t *testing.T) {
	withModsFolder(t, map[string]string{"a.sga": "local"})
	h := modsBehindOfficial(t, nil)

	resp := decodeList(t, serve(h, "POST", "api-dr.ageofempires.com", "/api/v4/Mods/Find/", []byte(capturedModQueries["/api/v4/Mods/Find/"])))
	if len(resp.ModList) != 1 {
		t.Fatalf("listed %d mods, want the local one", len(resp.ModList))
	}
}

func TestLocalModFileNeverProxied(t *testing.T) {
	withModsFolder(t, map[string]string{"a.sga": "local-bytes"})
	official := &fakeOfficial{status: http.StatusUnauthorized, body: officialUnauthorized}
	h := modsBehindOfficial(t, official)

	find := decodeList(t, serve(h, "POST", "api-dr.ageofempires.com", "/api/v4/Mods/Find/", []byte(`{}`)))
	before := official.hits()
	u, err := url.Parse(find.ModList[0].DownloadUrl)
	if err != nil {
		t.Fatal(err)
	}
	rr := serve(h, "GET", "api-dr.ageofempires.com", u.EscapedPath(), nil)
	if rr.Code != http.StatusOK || rr.Body.String() != "local-bytes" {
		t.Fatalf("File: %d %q", rr.Code, rr.Body.String())
	}
	if official.hits() != before {
		t.Fatal("the local file route was sent to the official service")
	}
}

func TestOfficialFirstRejectsOversizedBody(t *testing.T) {
	withModsFolder(t, nil)
	official := &fakeOfficial{status: http.StatusUnauthorized, body: officialUnauthorized}
	h := modsBehindOfficial(t, official)

	rr := serve(h, "POST", "api-dr.ageofempires.com", "/api/v4/Mods/Find/", bytes.Repeat([]byte(" "), maxOfficialFirstBody+1))
	if rr.Code != http.StatusRequestEntityTooLarge || official.hits() != 0 {
		t.Fatalf("status %d, official hits %d", rr.Code, official.hits())
	}
}

func TestLocalModsOnlyOnAoe4Host(t *testing.T) {
	withCanUseInternet(t, false)
	withModsFolder(t, nil)
	for _, gameId := range []string{game.AoE1, game.AoE2, game.AoE3, game.AoE4, game.AoM} {
		h := NewApiAgeOfEmpires().InitializeRoutes(gameId, http.NotFoundHandler())
		for _, path := range []string{"/api/v4/Mods/Find/", "/api/v4/mods/Find/"} {
			if rr := serve(h, "POST", "api.ageofempires.com", path, []byte(`{"game":4}`)); rr.Code != http.StatusNotFound {
				t.Fatalf("%s: api.ageofempires.com%s answered %d", gameId, path, rr.Code)
			}
		}
	}
}

func TestModsPathsKeepGlobalBodyCap(t *testing.T) {
	for _, path := range []string{"/api/v4/Mods/Publish/", "/api/v4/Mods/CheckAndPublishFile/", "/api/v4/Mods/Find/"} {
		if raisedBodySizePath(path) {
			t.Fatalf("%s skips the global body cap", path)
		}
	}
	withCanUseInternet(t, false)
	withModsFolder(t, nil)
	h := MaxBodySizeMiddleware(1<<20, NewAoe4ApiAgeOfEmpires().InitializeRoutes(game.AoE4, http.NotFoundHandler()))
	big := bytes.Repeat([]byte("x"), 2<<20)
	for _, tc := range []struct{ method, path string }{
		{"PUT", "/api/v4/Mods/CheckAndPublishFile/?modId=0"},
		{"POST", "/api/v4/Mods/Publish/"},
	} {
		if rr := serve(h, tc.method, "api-dr.ageofempires.com", tc.path, big); rr.Code == http.StatusOK {
			t.Fatalf("%s %s accepted an upload", tc.method, tc.path)
		}
	}
}
