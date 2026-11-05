package mods

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luskaner/ageServer/common/game"
)

var aoe4DateRegex = regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])-(0[1-9]|[1-2]\d|3[0-1])T([0-1]\d|2[0-3]):([0-5]\d):([0-5]\d)(?:[.,](\d{1,3}))?$`)

// Bodies captured from AoE IV (2026-11-04).
const (
	capturedMy        = `{"q":"","filter":0,"sort":"","order":"","start":1,"count":20000,"game":4,"modid":""}`
	capturedInstalled = `{"q":"","filter":0,"sort":"","order":"","start":0,"count":0,"game":4,"modid":""}`
	capturedFind      = `{"q":"","filter":0,"sort":"popular","order":"desc","start":0,"count":60,"game":4,"modid":""}`
)

var (
	t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

type fixture struct {
	t   *testing.T
	dir string
	mux *http.ServeMux
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ResetDefaultStore()
	orig := ModsFolder
	ModsFolder = t.TempDir()
	t.Cleanup(func() {
		ModsFolder = orig
		ResetDefaultStore()
	})
	mux := http.NewServeMux()
	for _, rt := range Routes() {
		mux.HandleFunc(rt.Method+" "+rt.Path, rt.Handler)
	}
	return &fixture{t: t, dir: filepath.Join(ModsFolder, game.AoE4), mux: mux}
}

func (f *fixture) write(name string, data []byte, mtime time.Time) {
	f.t.Helper()
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(f.dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) do(method, target, body, client string, headers ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "api-dr.ageofempires.com"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if client != "" {
		req.Header.Set("Authorization", client)
	}
	for k := 0; k+1 < len(headers); k += 2 {
		req.Header.Set(headers[k], headers[k+1])
	}
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

func (f *fixture) list(route, body, client string) ModListResponse {
	f.t.Helper()
	rr := f.do("POST", "/api/v4/Mods/"+route+"/", body, client)
	if rr.Code != http.StatusOK {
		f.t.Fatalf("%s: status %d (%s)", route, rr.Code, rr.Body.String())
	}
	var resp ModListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		f.t.Fatalf("%s: invalid JSON: %v (%s)", route, err, rr.Body.String())
	}
	if resp.ErrorCode != 0 || !resp.IsLoggedOn || resp.ModList == nil {
		f.t.Fatalf("%s: unexpected envelope: %+v", route, resp)
	}
	return resp
}

func (f *fixture) find(name string) ModItem {
	f.t.Helper()
	for _, m := range f.list("Find", capturedFind, "").ModList {
		if m.ModName == name {
			return m
		}
	}
	f.t.Fatalf("mod %q not listed", name)
	return ModItem{}
}

func names(items []ModItem) []string {
	out := make([]string, 0, len(items))
	for _, m := range items {
		out = append(out, m.ModName)
	}
	return out
}

func idStr(id int64) string { return strconv.FormatInt(id, 10) }

func filePath(t *testing.T, downloadUrl string) string {
	t.Helper()
	u, err := url.Parse(downloadUrl)
	if err != nil {
		t.Fatal(err)
	}
	return u.EscapedPath()
}

func TestCapturedQueriesWithoutMods(t *testing.T) {
	for _, tc := range []struct {
		name         string
		createFolder bool
	}{{"folder missing", false}, {"folder empty", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.createFolder {
				if err := os.MkdirAll(f.dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for route, body := range map[string]string{"My": capturedMy, "Installed": capturedInstalled, "Find": capturedFind} {
				rr := f.do("POST", "/api/v4/Mods/"+route+"/", body, "Steam1.0 TICKET")
				if rr.Code != http.StatusOK {
					t.Fatalf("%s: status %d", route, rr.Code)
				}
				if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Fatalf("%s: Content-Type %q", route, ct)
				}
				if !strings.Contains(rr.Body.String(), `"modList":[]`) {
					t.Fatalf("%s: modList must be an empty array, got %s", route, rr.Body.String())
				}
				if resp := f.list(route, body, ""); resp.TotalCount != 0 {
					t.Fatalf("%s: totalCount %d", route, resp.TotalCount)
				}
			}
		})
	}
}

func TestDiscoveryListsOnlyArchives(t *testing.T) {
	f := newFixture(t)
	payload := []byte("SGA_ARCHIVE_TEST_PAYLOAD")
	f.write("Big Map.sga", payload, t0)
	f.write("tuning.zip", []byte("zip"), t0)
	f.write("notes.txt", []byte("ignored"), t0)
	f.write("orphan.json", []byte(`{"modName":"no archive"}`), t0)
	if err := os.MkdirAll(filepath.Join(f.dir, "folder.sga"), 0o755); err != nil {
		t.Fatal(err)
	}

	resp := f.list("Find", capturedFind, "")
	if got := names(resp.ModList); !slices.Equal(got, []string{"Big Map", "tuning"}) || resp.TotalCount != 2 {
		t.Fatalf("listed %v (total %d), want [Big Map tuning]", got, resp.TotalCount)
	}
	m := resp.ModList[0]
	if m.ModFileSize != int64(len(payload)) || m.FileCRC != crc32.ChecksumIEEE(payload) {
		t.Fatalf("size/CRC: %d/%d", m.ModFileSize, m.FileCRC)
	}
	want := t0.Format(ModsTimeFormat)
	if m.CreateDate != want || m.LastUpdate != want || !aoe4DateRegex.MatchString(m.LastUpdate) {
		t.Fatalf("dates: create=%q update=%q, want %q", m.CreateDate, m.LastUpdate, want)
	}
	if m.IsOwner || m.Ratings.TotalCount != 0 || m.CurrentRating != 0 || m.Downloads != 0 {
		t.Fatalf("fabricated ownership/ratings/downloads: %+v", m)
	}
	if !m.IsInstalled || !m.UserInstalled {
		t.Fatal("a local mod must start subscribed")
	}
	if wantUrl := "https://api-dr.ageofempires.com/api/v4/Mods/File/" + idStr(m.ModId) + "/Big%20Map.sga"; m.DownloadUrl != wantUrl {
		t.Fatalf("downloadUrl %q, want %q", m.DownloadUrl, wantUrl)
	}
}

func TestDownloadFileStreamsTheArchive(t *testing.T) {
	f := newFixture(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 16<<10) // 256 KiB
	f.write("big.sga", payload, t0)
	m := f.find("big")
	sum := md5.Sum(payload)

	rr := f.do("GET", filePath(t, m.DownloadUrl), "", "")
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), payload) {
		t.Fatalf("status %d, %d bytes", rr.Code, rr.Body.Len())
	}
	if got := rr.Header().Get("ETag"); got != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Fatalf("ETag %q", got)
	}
	if got := rr.Header().Get("Content-MD5"); got != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatalf("Content-MD5 %q", got)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "big.sga") {
		t.Fatalf("Content-Disposition %q", got)
	}

	rr = f.do("GET", filePath(t, m.DownloadUrl), "", "", "Range", "bytes=10-19")
	if rr.Code != http.StatusPartialContent || !bytes.Equal(rr.Body.Bytes(), payload[10:20]) {
		t.Fatalf("range: status %d body %q", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-MD5") != "" {
		t.Fatal("Content-MD5 must not describe a partial body")
	}
}

func TestIdsSurviveRestartAndUpdates(t *testing.T) {
	f := newFixture(t)
	f.write("a.sga", []byte("v1"), t0)
	first := f.find("a")

	ResetDefaultStore() // server restart
	if again := f.find("a"); again.ModId != first.ModId {
		t.Fatalf("id changed across restart: %d -> %d", first.ModId, again.ModId)
	}

	v2 := bytes.Repeat([]byte("v2"), 100)
	f.write("a.sga", v2, t1)
	updated := f.find("a")
	if updated.ModId != first.ModId {
		t.Fatalf("id changed on update: %d -> %d", first.ModId, updated.ModId)
	}
	if updated.ModFileSize != int64(len(v2)) || updated.FileCRC != crc32.ChecksumIEEE(v2) || updated.LastUpdate != t1.Format(ModsTimeFormat) {
		t.Fatalf("stale metadata after update: %+v", updated)
	}
	rr := f.do("GET", filePath(t, updated.DownloadUrl), "", "")
	if !bytes.Equal(rr.Body.Bytes(), v2) {
		t.Fatalf("download served %d bytes, want the updated %d", rr.Body.Len(), len(v2))
	}
}

func TestRemovedModDisappears(t *testing.T) {
	f := newFixture(t)
	f.write("a.sga", []byte("a"), t0)
	f.write("b.sga", []byte("b"), t0)
	a := f.find("a")

	if err := os.Remove(filepath.Join(f.dir, "a.sga")); err != nil {
		t.Fatal(err)
	}
	if got := names(f.list("Find", capturedFind, "").ModList); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("listed %v after removal", got)
	}
	for _, target := range []string{"/api/v4/Mods/Detail/" + idStr(a.ModId), filePath(t, a.DownloadUrl)} {
		if rr := f.do("GET", target, "", ""); rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s: status %d, want 404", target, rr.Code)
		}
	}
}

func TestSidecarOverridesAndReloads(t *testing.T) {
	f := newFixture(t)
	f.write("pop.sga", []byte("data"), t0)
	f.write("pop.json", []byte(`{"modName":"Population 500","modDescription":"Raises the cap","modType":"map","creatorName":"Luiz","modVersion":"2.1"}`), t0)

	m := f.find("Population 500")
	if m.ModDescription != "Raises the cap" || m.ModType != "proceduralmap" || !slices.Equal(m.ModTagNames, []string{"proceduralmap"}) ||
		m.CreatorName != "Luiz" || m.ModVersion != "2.1" {
		t.Fatalf("sidecar not applied: %+v", m)
	}

	f.write("pop.json", []byte(`{"modName":"Population 1000"}`), t1)
	if again := f.find("Population 1000"); again.ModId != m.ModId {
		t.Fatalf("sidecar edit changed the id: %d -> %d", m.ModId, again.ModId)
	}
}

func TestInstalledIsConsistentPerClient(t *testing.T) {
	f := newFixture(t)
	f.write("a.sga", []byte("a"), t0)
	f.write("b.sga", []byte("b"), t0)
	a := f.find("a")
	const alice, bob = "Steam1.0 ALICE", "Steam1.0 BOB"

	installed := func(client string) []string {
		return names(f.list("Installed", capturedInstalled, client).ModList)
	}
	if got := installed(alice); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("initial Installed %v", got)
	}

	rr := f.do("POST", "/api/v4/Mods/Unsubscribe/"+idStr(a.ModId), "", alice)
	var action ActionResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &action) != nil || action.ResultValue != idStr(a.ModId) {
		t.Fatalf("Unsubscribe: %d %s", rr.Code, rr.Body.String())
	}
	if got := installed(alice); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("Installed after unsubscribing a: %v", got)
	}
	for _, m := range f.list("Find", capturedFind, alice).ModList {
		if m.IsInstalled != (m.ModName == "b") {
			t.Fatalf("Find isInstalled for %s = %v", m.ModName, m.IsInstalled)
		}
	}
	if got := installed(bob); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("another client was affected: %v", got)
	}

	if rr := f.do("POST", "/api/v4/Mods/Subscribe/", `{"modid":"`+idStr(a.ModId)+`"}`, alice); rr.Code != http.StatusOK {
		t.Fatalf("Subscribe: %d", rr.Code)
	}
	if got := installed(alice); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Installed after subscribing again: %v", got)
	}
}

func TestMyIsAlwaysEmpty(t *testing.T) {
	f := newFixture(t)
	f.write("a.sga", []byte("a"), t0)
	if resp := f.list("My", capturedMy, ""); len(resp.ModList) != 0 || resp.TotalCount != 0 {
		t.Fatalf("My listed %v", names(resp.ModList))
	}
}

func TestUnknownOrMissingIdIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.write("a.sga", []byte("a"), t0)
	if f.find("a").ModId == 999 {
		t.Skip("id collision with the probe id")
	}
	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/api/v4/Mods/Detail/999", ""},
		{"POST", "/api/v4/Mods/Detail/", `{}`},
		{"POST", "/api/v4/Mods/Detail/", `{"modid":"999"}`},
		{"GET", "/api/v4/Mods/Download/999", ""},
		{"POST", "/api/v4/Mods/Download/", `{"game":4}`},
		{"GET", "/api/v4/Mods/File/999/a.sga", ""},
		{"POST", "/api/v4/Mods/Subscribe/", `{}`},
		{"POST", "/api/v4/Mods/Unsubscribe/999", ""},
	} {
		rr := f.do(tc.method, tc.target, tc.body, "")
		var resp ErrorResponse
		if rr.Code != http.StatusNotFound || json.Unmarshal(rr.Body.Bytes(), &resp) != nil || resp.ErrorCode != errorCodeNotFound {
			t.Fatalf("%s %s %s: status %d body %s", tc.method, tc.target, tc.body, rr.Code, rr.Body.String())
		}
	}
}

func TestDetailAndDownloadAcceptEveryIdForm(t *testing.T) {
	f := newFixture(t)
	payload := []byte("payload")
	f.write("a.sga", payload, t0)
	id := idStr(f.find("a").ModId)

	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/api/v4/Mods/Detail/" + id, ""},
		{"GET", "/api/v4/Mods/Detail?modId=" + id, ""},
		{"POST", "/api/v4/Mods/Detail/", `{"modid":"` + id + `","game":4}`},
		{"POST", "/api/v4/Mods/Detail", `{"modId":` + id + `}`},
	} {
		rr := f.do(tc.method, tc.target, tc.body, "")
		var item ModItem
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &item) != nil || idStr(item.ModId) != id {
			t.Fatalf("%s %s %s: %d %s", tc.method, tc.target, tc.body, rr.Code, rr.Body.String())
		}
	}

	rr := f.do("GET", "/api/v4/Mods/Download/"+id, "", "")
	var dl DownloadResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &dl) != nil {
		t.Fatalf("Download: %d %s", rr.Code, rr.Body.String())
	}
	if dl.FileCRC != crc32.ChecksumIEEE(payload) || dl.ModFileSize != int64(len(payload)) || !strings.Contains(dl.DownloadUrl, "/api/v4/Mods/File/"+id+"/") {
		t.Fatalf("DownloadResponse %+v", dl)
	}
}

func TestPaginationUsesStartAndCount(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"c", "a", "b"} {
		f.write(n+".sga", []byte(n), t0)
	}
	for _, tc := range []struct {
		body string
		want []string
	}{
		{`{"start":1,"count":1}`, []string{"b"}},
		{`{"start":0,"count":0}`, []string{"a", "b", "c"}},
		{`{"start":-5,"count":2}`, []string{"a", "b"}},
		{`{"start":10,"count":60}`, []string{}},
	} {
		resp := f.list("Find", tc.body, "")
		if got := names(resp.ModList); !slices.Equal(got, tc.want) || resp.TotalCount != 3 {
			t.Fatalf("%s: got %v (total %d), want %v (total 3)", tc.body, got, resp.TotalCount, tc.want)
		}
	}
}

func TestQueryGameAndIdFilters(t *testing.T) {
	f := newFixture(t)
	f.write("Big Map.sga", []byte("1"), t0)
	f.write("small.sga", []byte("2"), t0)
	small := f.find("small")

	for _, tc := range []struct {
		body string
		want []string
	}{
		{`{"q":"BIG","game":4}`, []string{"Big Map"}},
		{`{"game":2}`, []string{}},
		{`{"modid":"` + idStr(small.ModId) + `"}`, []string{"small"}},
	} {
		if got := names(f.list("Find", tc.body, "").ModList); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestRoutesHaveNoUploadsAndOnlyFileIsLocalOnly(t *testing.T) {
	var routes []string
	for _, rt := range Routes() {
		key := rt.Method + " " + rt.Path
		routes = append(routes, key)
		if rt.LocalOnly != strings.Contains(rt.Path, "/File/") {
			t.Fatalf("%s: LocalOnly = %v", key, rt.LocalOnly)
		}
	}
	for _, want := range []string{"POST /api/v4/Mods/My/", "POST /api/v4/Mods/Installed/", "POST /api/v4/Mods/Find/"} {
		if !slices.Contains(routes, want) {
			t.Fatalf("missing captured route %q", want)
		}
	}
	for _, r := range routes {
		for _, banned := range []string{"Publish", "CheckAndPublishFile", "Delete", "PUT "} {
			if strings.Contains(r, banned) {
				t.Fatalf("unexpected route %q", r)
			}
		}
	}
}
