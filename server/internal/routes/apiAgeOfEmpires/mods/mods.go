// Package mods answers the Age of Empires IV mod browser from local archives
// when the official mod service cannot be used (issue #378): without Internet,
// or when it rejects the client (401, e.g. not using the official launcher).
// The router decides which; this package only serves the local side.
//
// The game asks api-dr.ageofempires.com/api/v4/Mods/{My,Installed,Find}/ for
// its lists; those three requests were captured from the game. The item schema
// and the Detail, Download, File, Subscribe and Unsubscribe routes are a
// best-effort match still to be confirmed in-game.
//
// Mods are the .sga/.zip files in <ModsFolder>/age4, rescanned on every request
// (only new or changed files are hashed). An optional sidecar <name>.json
// overrides the metadata. Every mod starts subscribed for every client, so
// dropping a file in the folder is enough for all players to get it;
// Unsubscribe opts a client out (in memory) until it subscribes again.
package mods

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luskaner/ageServer/common"
	"github.com/luskaner/ageServer/common/game"
	"github.com/luskaner/ageServer/common/paths"
	i "github.com/luskaner/ageServer/server/internal"
)

// ModsTimeFormat matches the strict timestamp regex AoE IV enforces on mod dates:
// ^(\d{4})-(0[1-9]|1[0-2])-(0[1-9]|[1-2]\d|3[0-1])T([0-1]\d|2[0-3]):([0-5]\d):([0-5]\d)(?:[.,](\d{1,3}))?$
const ModsTimeFormat = "2006-01-02T15:04:05.000"

const (
	routePrefix   = "/api/v4/Mods"
	gameTitleId   = 4
	gameTitleName = "Age of Empires IV"
	// maxRequestBody bounds the JSON a handler parses; every request the game
	// was seen sending is under 1 KiB.
	maxRequestBody    = 64 << 10
	errorCodeNotFound = 404
)

// ModsFolder is the base directory; AoE IV mods live in <ModsFolder>/age4.
var ModsFolder = filepath.Join(paths.ResourcesDir, "mods")

type SearchParams struct {
	Query  string `json:"q"`
	Filter int    `json:"filter"`
	Sort   string `json:"sort"`
	Order  string `json:"order"`
	Start  int    `json:"start"`
	Count  int    `json:"count"`
	Game   int    `json:"game"`
	ModId  string `json:"modid"`
}

type ModImage struct {
	ImageId        string `json:"imageId"`
	DisplayIndex   int    `json:"displayIndex"`
	ImageName      string `json:"imageName"`
	ImageThumbnail string `json:"imageThumbnail"`
	ImageFileSize  int64  `json:"imageFileSize"`
	FileUrl        string `json:"fileUrl"`
}

type ModRatings struct {
	Average    float64 `json:"average"`
	TotalCount int     `json:"totalCount"`
	Count1Star int     `json:"count1Star"`
	Count2Star int     `json:"count2Star"`
	Count3Star int     `json:"count3Star"`
	Count4Star int     `json:"count4Star"`
	Count5Star int     `json:"count5Star"`
}

type ModScanResult struct {
	ResultJson string `json:"resultJson"`
}

type ModItem struct {
	ModId                int64      `json:"modId"`
	ItemId               string     `json:"itemId"`
	ClientId             string     `json:"clientId"`
	ModName              string     `json:"modName"`
	GameTitleId          int        `json:"gameTitleId"`
	GameTitleName        string     `json:"gameTitleName"`
	VerifiedVersion      string     `json:"verifiedVersion"`
	ItemVersion          string     `json:"itemVersion"`
	ModVersion           string     `json:"modVersion"`
	ModType              string     `json:"modType"`
	ModTypeId            int        `json:"modTypeId"`
	ModStatus            string     `json:"modStatus"`
	ModStatusId          int        `json:"modStatusId"`
	ModStatusMessage     string     `json:"modStatusMessage"`
	ModVisibility        string     `json:"modVisibility"`
	Description          string     `json:"description"`
	ModDescription       string     `json:"modDescription"`
	ChangeList           string     `json:"changeList"`
	ImageUrls            []ModImage `json:"imageUrls"`
	CreatorId            string     `json:"creatorId"`
	CreatorName          string     `json:"creatorName"`
	CreateDate           string     `json:"createDate"`
	LastUpdate           string     `json:"lastUpdate"`
	Submitted            string     `json:"submitted"`
	UpdateAvailable      bool       `json:"updateAvailable"`
	ModFileSize          int64      `json:"modFileSize"`
	ModDiskSize          int64      `json:"modDiskSize"`
	FileCRC              uint32     `json:"fileCRC"`
	Downloads            int        `json:"downloads"`
	DownloadCount        int        `json:"downloadCount"`
	Installs             int        `json:"installs"`
	InstallCount         int        `json:"installCount"`
	Ratings              ModRatings `json:"ratings"`
	CurrentRating        float64    `json:"currentRating"`
	UserRating           int        `json:"userRating"`
	PositiveRatings      int        `json:"positiveRatings"`
	ReviewsCount         int        `json:"reviewsCount"`
	LeastFavorableReview any        `json:"leastFavorableReview"`
	MostFavorableReview  any        `json:"mostFavorableReview"`
	IsOwner              bool       `json:"isOwner"`
	IsLoggedOn           bool       `json:"isLoggedOn"`
	IsInstalled          bool       `json:"isInstalled"`
	UserDownloaded       bool       `json:"userDownloaded"`
	UserInstalled        bool       `json:"userInstalled"`
	UserFlagged          bool       `json:"userFlagged"`
	Active               bool       `json:"active"`
	Thumbnail            string     `json:"thumbnail"`
	DownloadUrl          string     `json:"downloadUrl"`
	ModTagIds            []int      `json:"modTagIds"`
	ModTagNames          []string   `json:"modTagNames"`
	ErrorCode            int        `json:"errorCode"`
	ErrorMessage         string     `json:"errorMessage"`
}

type ModListResponse struct {
	SearchParams      *SearchParams `json:"searchParams"`
	ModList           []ModItem     `json:"modList"`
	TotalCount        int           `json:"totalCount"`
	ContinuationToken string        `json:"continuationToken"`
	IsLoggedOn        bool          `json:"isLoggedOn"`
	ErrorCode         int           `json:"errorCode"`
	ErrorMessage      string        `json:"errorMessage"`
}

type DownloadResponse struct {
	DownloadUrl   string `json:"downloadUrl"`
	DownloadCount int    `json:"downloadCount"`
	InstallCount  int    `json:"installCount"`
	FileCRC       uint32 `json:"fileCRC"`
	ModFileSize   int64  `json:"modFileSize"`
	ModDiskSize   int64  `json:"modDiskSize"`
	ErrorCode     int    `json:"errorCode"`
	ErrorMessage  string `json:"errorMessage"`
}

type ActionResponse struct {
	ErrorCode       int           `json:"errorCode"`
	ErrorMessage    string        `json:"errorMessage"`
	ResultKey       string        `json:"resultKey"`
	ResultValue     string        `json:"resultValue"`
	StringValue     string        `json:"stringValue"`
	BoolValue       bool          `json:"boolValue"`
	Status          bool          `json:"status"`
	Code            int           `json:"code"`
	Location        string        `json:"location"`
	FilteredResults []string      `json:"filteredResults"`
	ModScanResult   ModScanResult `json:"modScanResult"`
}

type ErrorResponse struct {
	ErrorCode    int    `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// Route is one local mod endpoint. LocalOnly routes exist only here (the
// official service has no equivalent); the rest mirror official endpoints.
type Route struct {
	Method    string
	Path      string
	Handler   http.HandlerFunc
	LocalOnly bool
}

// Routes lists every local mod endpoint with its full path.
func Routes() []Route {
	var routes []Route
	for _, rt := range []struct {
		path     string
		methods  []string
		handler  http.HandlerFunc
		suffixes []string
	}{
		{"/My", []string{http.MethodPost}, My, []string{"", "/"}},
		{"/Installed", []string{http.MethodPost}, Installed, []string{"", "/"}},
		{"/Find", []string{http.MethodPost}, Find, []string{"", "/"}},
		{"/Detail", []string{http.MethodGet, http.MethodPost}, Detail, []string{"", "/", "/{id}"}},
		{"/Download", []string{http.MethodGet, http.MethodPost}, Download, []string{"", "/", "/{id}"}},
		{"/Subscribe", []string{http.MethodPost}, Subscribe, []string{"", "/", "/{id}"}},
		{"/Unsubscribe", []string{http.MethodPost}, Unsubscribe, []string{"", "/", "/{id}"}},
	} {
		for _, method := range rt.methods {
			for _, suffix := range rt.suffixes {
				routes = append(routes, Route{Method: method, Path: routePrefix + rt.path + suffix, Handler: rt.handler})
			}
		}
	}
	return append(routes, Route{Method: http.MethodGet, Path: routePrefix + "/File/{id}/{filename}", Handler: DownloadFile, LocalOnly: true})
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type localMod struct {
	item           ModItem // shared metadata; per-client fields are set on copies
	path           string
	filename       string
	size           int64
	modTime        time.Time
	sidecarModTime time.Time
	md5            string
}

type Store struct {
	scanMu sync.Mutex // one scan at a time; hashing runs without holding mu
	mu     sync.RWMutex
	byPath map[string]*localMod
	byId   map[int64]*localMod
	optOut map[string]map[int64]bool // client -> mods it unsubscribed from
}

var defaultStore = NewStore()

func NewStore() *Store {
	return &Store{
		byPath: make(map[string]*localMod),
		byId:   make(map[int64]*localMod),
		optOut: make(map[string]map[int64]bool),
	}
}

func DefaultStore() *Store {
	return defaultStore
}

func ResetDefaultStore() {
	defaultStore = NewStore()
}

type diskEntry struct {
	path           string
	name           string
	size           int64
	modTime        time.Time
	sidecarPath    string
	sidecarModTime time.Time
}

// listArchives returns the .sga/.zip files of dir by path, each with its
// optional sidecar <name>.json.
func listArchives(dir string) map[string]diskEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type sidecarInfo struct {
		path    string
		modTime time.Time
	}
	sidecars := make(map[string]sidecarInfo)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		if info, err := e.Info(); err == nil {
			sidecars[strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))] = sidecarInfo{filepath.Join(dir, name), info.ModTime()}
		}
	}
	archives := make(map[string]diskEntry)
	for _, e := range entries {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if e.IsDir() || (ext != ".sga" && ext != ".zip") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		entry := diskEntry{path: filepath.Join(dir, name), name: name, size: info.Size(), modTime: info.ModTime()}
		if sc, ok := sidecars[strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))]; ok {
			entry.sidecarPath, entry.sidecarModTime = sc.path, sc.modTime
		}
		archives[entry.path] = entry
	}
	return archives
}

func hashFile(path string) (uint32, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()
	crc := crc32.NewIEEE()
	sum := md5.New()
	if _, err = io.Copy(io.MultiWriter(crc, sum), f); err != nil {
		return 0, "", err
	}
	return crc.Sum32(), hex.EncodeToString(sum.Sum(nil)), nil
}

// sync brings the store in line with the folder: new and changed archives are
// (re)hashed, removed ones are dropped. A file whose size, modification time
// or sidecar did not change is not read again.
func (s *Store) sync() {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	onDisk := listArchives(filepath.Join(ModsFolder, game.AoE4))

	s.mu.RLock()
	var changed []diskEntry
	for _, e := range onDisk {
		m, ok := s.byPath[e.path]
		if !ok || m.size != e.size || !m.modTime.Equal(e.modTime) || !m.sidecarModTime.Equal(e.sidecarModTime) {
			changed = append(changed, e)
		}
	}
	var removed []string
	for path := range s.byPath {
		if _, ok := onDisk[path]; !ok {
			removed = append(removed, path)
		}
	}
	s.mu.RUnlock()
	if len(changed) == 0 && len(removed) == 0 {
		return
	}

	type scanned struct {
		entry diskEntry
		crc   uint32
		md5   string
		meta  sidecar
	}
	results := make([]scanned, 0, len(changed))
	for _, e := range changed {
		crc, sum, err := hashFile(e.path)
		if err != nil {
			// Unreadable for now (e.g. still being copied): retried next scan.
			continue
		}
		results = append(results, scanned{e, crc, sum, readSidecar(e.sidecarPath)})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range removed {
		delete(s.byId, s.byPath[path].item.ModId)
		delete(s.byPath, path)
	}
	for _, r := range results {
		m, ok := s.byPath[r.entry.path]
		if !ok {
			m = &localMod{path: r.entry.path, filename: r.entry.name}
			m.item.ModId = s.freeIdLocked(r.entry.name)
			s.byPath[m.path] = m
			s.byId[m.item.ModId] = m
		}
		m.size, m.modTime, m.sidecarModTime, m.md5 = r.entry.size, r.entry.modTime, r.entry.sidecarModTime, r.md5
		m.item = buildItem(m.item.ModId, r.entry, r.crc, r.meta)
	}
}

// freeIdLocked derives the id from the file name, so it survives restarts and
// content updates: the game may remember the ids of the mods it already has.
func (s *Store) freeIdLocked(filename string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(filename)))
	id := int64(h.Sum32() & 0x7fffffff)
	if id == 0 {
		id = 1
	}
	for {
		if _, taken := s.byId[id]; !taken {
			return id
		}
		id++
	}
}

type sidecar struct {
	ModName        string   `json:"modName"`
	ModDescription string   `json:"modDescription"`
	ModType        string   `json:"modType"`
	ModTypeId      int      `json:"modTypeId"`
	CreatorName    string   `json:"creatorName"`
	ModVersion     string   `json:"modVersion"`
	ModTagNames    []string `json:"modTagNames"`
}

func readSidecar(path string) (meta sidecar) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxRequestBody {
		return
	}
	_ = json.Unmarshal(data, &meta)
	return
}

func normalizeModType(modType string) string {
	switch strings.ToLower(strings.TrimSpace(modType)) {
	case "proceduralmap", "map", "generatedmap":
		return "proceduralmap"
	case "gamemode", "mode", "scenario":
		return "gamemode"
	default:
		return "tuning"
	}
}

func buildItem(id int64, e diskEntry, crc uint32, meta sidecar) ModItem {
	date := e.modTime.UTC().Format(ModsTimeFormat)
	idStr := strconv.FormatInt(id, 10)
	name := strings.TrimSuffix(e.name, filepath.Ext(e.name))
	item := ModItem{
		ModId:           id,
		ItemId:          idStr,
		ClientId:        idStr,
		ModName:         name,
		GameTitleId:     gameTitleId,
		GameTitleName:   gameTitleName,
		VerifiedVersion: "1.0",
		ItemVersion:     "1.0",
		ModVersion:      "1.0",
		ModType:         "tuning",
		ModTypeId:       1,
		ModStatus:       "Published",
		ModStatusId:     1,
		ModVisibility:   "Public",
		Description:     name,
		ModDescription:  name,
		ImageUrls:       []ModImage{},
		CreatorId:       "0",
		CreatorName:     "LAN Server",
		CreateDate:      date,
		LastUpdate:      date,
		Submitted:       date,
		ModFileSize:     e.size,
		ModDiskSize:     e.size,
		FileCRC:         crc,
		IsLoggedOn:      true,
		Active:          true,
		ModTagIds:       []int{1},
		ModTagNames:     []string{"tuning"},
	}
	if meta.ModName != "" {
		item.ModName = meta.ModName
	}
	if meta.ModDescription != "" {
		item.Description = meta.ModDescription
		item.ModDescription = meta.ModDescription
	}
	if meta.ModType != "" {
		item.ModType = normalizeModType(meta.ModType)
		item.ModTagNames = []string{item.ModType}
	}
	if meta.ModTypeId > 0 {
		item.ModTypeId = meta.ModTypeId
	}
	if meta.CreatorName != "" {
		item.CreatorName = meta.CreatorName
	}
	if meta.ModVersion != "" {
		item.ModVersion = meta.ModVersion
		item.ItemVersion = meta.ModVersion
		item.VerifiedVersion = meta.ModVersion
	}
	if len(meta.ModTagNames) > 0 {
		item.ModTagNames = meta.ModTagNames
	}
	return item
}

// viewLocked is what one client sees of a mod. Requires s.mu (read).
func (s *Store) viewLocked(r *http.Request, m *localMod, key string) ModItem {
	item := m.item
	subscribed := !s.optOut[key][item.ModId]
	item.DownloadUrl = downloadURL(r, item.ModId, m.filename)
	item.IsInstalled = subscribed
	item.UserInstalled = subscribed
	item.UserDownloaded = subscribed
	return item
}

// view syncs and returns one mod as the requesting client sees it.
func (s *Store) view(r *http.Request, id int64) (ModItem, bool) {
	s.sync()
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.byId[id]
	if !ok {
		return ModItem{}, false
	}
	return s.viewLocked(r, m, clientKey(r)), true
}

func (s *Store) list(r *http.Request, sp SearchParams, subscribedOnly bool) ModListResponse {
	s.sync()
	key := clientKey(r)
	query := strings.ToLower(strings.TrimSpace(sp.Query))
	filterId, hasFilterId := parseId(sp.ModId)

	matches := make([]ModItem, 0)
	if sp.Game == 0 || sp.Game == gameTitleId {
		s.mu.RLock()
		for _, m := range s.byId {
			if subscribedOnly && s.optOut[key][m.item.ModId] {
				continue
			}
			if hasFilterId && m.item.ModId != filterId {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(m.item.ModName), query) &&
				!strings.Contains(strings.ToLower(m.item.ModDescription), query) {
				continue
			}
			matches = append(matches, s.viewLocked(r, m, key))
		}
		s.mu.RUnlock()
	}
	// No download or rating data exists locally, so every sort is by name.
	sort.Slice(matches, func(a, b int) bool {
		na, nb := strings.ToLower(matches[a].ModName), strings.ToLower(matches[b].ModName)
		if na != nb {
			return na < nb
		}
		return matches[a].ModId < matches[b].ModId
	})
	return listResponse(sp, paginate(matches, sp.Start, sp.Count), len(matches))
}

// paginate treats start as an offset and a non-positive count as "all".
func paginate(items []ModItem, start, count int) []ModItem {
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return []ModItem{}
	}
	items = items[start:]
	if count > 0 && count < len(items) {
		items = items[:count]
	}
	return items
}

func listResponse(sp SearchParams, page []ModItem, total int) ModListResponse {
	return ModListResponse{
		SearchParams: &sp,
		ModList:      page,
		TotalCount:   total,
		IsLoggedOn:   true,
	}
}

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

type request struct {
	params SearchParams
	fields map[string]any
}

func readRequest(r *http.Request) request {
	req := request{params: SearchParams{Game: gameTitleId}}
	if r.Body == nil {
		return req
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	_ = r.Body.Close()
	if len(bytes.TrimSpace(raw)) == 0 {
		return req
	}
	_ = json.Unmarshal(raw, &req.params)
	_ = json.Unmarshal(raw, &req.fields)
	return req
}

func parseId(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id, err == nil && id > 0
}

// modId looks for the id in the path, the query string and the JSON body.
func modId(r *http.Request, req request) (int64, bool) {
	candidates := []string{
		r.PathValue("id"),
		r.URL.Query().Get("modId"),
		r.URL.Query().Get("modid"),
		r.URL.Query().Get("id"),
	}
	for _, key := range []string{"modId", "modid", "ModId", "id", "itemId"} {
		switch v := req.fields[key].(type) {
		case string:
			candidates = append(candidates, v)
		case float64:
			candidates = append(candidates, strconv.FormatFloat(v, 'f', -1, 64))
		}
	}
	for _, c := range candidates {
		if id, ok := parseId(c); ok {
			return id, true
		}
	}
	return 0, false
}

// clientKey identifies the client for its subscriptions: the platform ticket
// when there is one, the IP otherwise.
func clientKey(r *http.Request) string {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		return auth
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func downloadURL(r *http.Request, id int64, filename string) string {
	host := r.Host
	if host == "" {
		host = common.Aoe4ApiAgeOfEmpires
	}
	u := url.URL{
		Scheme: "https",
		Host:   host,
		Path:   fmt.Sprintf("%s/File/%d/%s", routePrefix, id, filename),
	}
	return u.String()
}

func writeNotFound(w http.ResponseWriter) {
	encoded, _ := json.Marshal(ErrorResponse{ErrorCode: errorCodeNotFound, ErrorMessage: "Mod not found."})
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(append(encoded, '\n'))
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// My handles POST /api/v4/Mods/My/ (MyModsQueryAsync): the mods the user
// published, which locally is always none.
func My(w http.ResponseWriter, r *http.Request) {
	req := readRequest(r)
	i.JSON(&w, listResponse(req.params, []ModItem{}, 0))
}

// Installed handles POST /api/v4/Mods/Installed/ (RetrieveSubscribedQueryAsync).
func Installed(w http.ResponseWriter, r *http.Request) {
	req := readRequest(r)
	i.JSON(&w, defaultStore.list(r, req.params, true))
}

// Find handles POST /api/v4/Mods/Find/ (SearchQueryAsync).
func Find(w http.ResponseWriter, r *http.Request) {
	req := readRequest(r)
	i.JSON(&w, defaultStore.list(r, req.params, false))
}

// Detail handles /api/v4/Mods/Detail/{id} and /api/v4/Mods/Detail/ with the id
// in the query string or the body.
func Detail(w http.ResponseWriter, r *http.Request) {
	id, ok := modId(r, readRequest(r))
	if !ok {
		writeNotFound(w)
		return
	}
	item, ok := defaultStore.view(r, id)
	if !ok {
		writeNotFound(w)
		return
	}
	i.JSON(&w, item)
}

// Download handles /api/v4/Mods/Download/{id} and /api/v4/Mods/Download/.
func Download(w http.ResponseWriter, r *http.Request) {
	id, ok := modId(r, readRequest(r))
	if !ok {
		writeNotFound(w)
		return
	}
	item, ok := defaultStore.view(r, id)
	if !ok {
		writeNotFound(w)
		return
	}
	i.JSON(&w, DownloadResponse{
		DownloadUrl:   item.DownloadUrl,
		DownloadCount: item.DownloadCount,
		InstallCount:  item.InstallCount,
		FileCRC:       item.FileCRC,
		ModFileSize:   item.ModFileSize,
		ModDiskSize:   item.ModDiskSize,
	})
}

// DownloadFile serves GET /api/v4/Mods/File/{id}/{filename}, streaming the
// archive from disk (with Range support).
func DownloadFile(w http.ResponseWriter, r *http.Request) {
	id, ok := parseId(r.PathValue("id"))
	if !ok {
		writeNotFound(w)
		return
	}
	defaultStore.sync()
	defaultStore.mu.RLock()
	m, found := defaultStore.byId[id]
	var path, filename, sum string
	var size int64
	var modTime time.Time
	if found {
		path, filename, sum, size, modTime = m.path, m.filename, m.md5, m.size, m.modTime
	}
	defaultStore.mu.RUnlock()
	if !found {
		writeNotFound(w)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		writeNotFound(w)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.Size() != size || !info.ModTime().Equal(modTime) {
		// Changed since the scan: it would not match the CRC the game was given.
		http.Error(w, "Mod is being updated, try again.", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("ETag", `"`+sum+`"`)
	if r.Header.Get("Range") == "" {
		if raw, err := hex.DecodeString(sum); err == nil {
			w.Header().Set("Content-MD5", base64.StdEncoding.EncodeToString(raw))
		}
	}
	http.ServeContent(w, r, filename, modTime, f)
}

// Subscribe handles /api/v4/Mods/Subscribe/{id} and /api/v4/Mods/Subscribe/.
func Subscribe(w http.ResponseWriter, r *http.Request) {
	setSubscribed(w, r, true)
}

// Unsubscribe handles /api/v4/Mods/Unsubscribe/{id} and /api/v4/Mods/Unsubscribe/.
func Unsubscribe(w http.ResponseWriter, r *http.Request) {
	setSubscribed(w, r, false)
}

func setSubscribed(w http.ResponseWriter, r *http.Request, subscribed bool) {
	id, ok := modId(r, readRequest(r))
	if !ok {
		writeNotFound(w)
		return
	}
	item, ok := defaultStore.view(r, id)
	if !ok {
		writeNotFound(w)
		return
	}
	key := clientKey(r)
	defaultStore.mu.Lock()
	if subscribed {
		delete(defaultStore.optOut[key], id)
		if len(defaultStore.optOut[key]) == 0 {
			delete(defaultStore.optOut, key)
		}
	} else {
		if defaultStore.optOut[key] == nil {
			defaultStore.optOut[key] = make(map[int64]bool)
		}
		defaultStore.optOut[key][id] = true
	}
	defaultStore.mu.Unlock()

	idStr := strconv.FormatInt(id, 10)
	i.JSON(&w, ActionResponse{
		ResultKey:       "ModId",
		ResultValue:     idStr,
		StringValue:     idStr,
		BoolValue:       true,
		Status:          true,
		Location:        item.DownloadUrl,
		FilteredResults: []string{},
		ModScanResult:   ModScanResult{ResultJson: "{}"},
	})
}
