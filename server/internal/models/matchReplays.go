package models

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luskaner/ageServer/common/paths"
	i "github.com/luskaner/ageServer/server/internal"
	"github.com/luskaner/ageServer/server/internal/logger"
)

// MatchReplays keeps the replay (and stats) blobs AoE IV uploads after a match
// on this server's disk, so they can be downloaded later from the match
// history. The game is told to upload them here, to the same host it already
// talks to; nothing is ever sent to the official (Microsoft) storage.
//
// Uploads and downloads need a token the server handed out: an upload token is
// only issued to a player of a match hosted here, for that match's two blobs.
// The first complete upload of a blob is kept; later ones are discarded.
// The index is rebuilt from the folder at startup, so replays survive restarts.

// ReplayKind is one of the two blobs a match can have.
type ReplayKind int

const (
	ReplayKindReplay ReplayKind = 0
	ReplayKindStats  ReplayKind = 1
)

var replayKindFiles = map[ReplayKind]string{
	ReplayKindReplay: "replay.rec.gz",
	ReplayKindStats:  "stats.rgs.gz",
}

// ReplayKinds lists every kind, replay first.
var ReplayKinds = []ReplayKind{ReplayKindReplay, ReplayKindStats}

// Filename is the blob name of the kind.
func (k ReplayKind) Filename() string {
	return replayKindFiles[k]
}

const (
	matchReplaysSubfolder = "match-replays"
	replayKeyPrefix       = "replays/"
	// Upload tokens outlive the match report long enough for a slow upload to
	// start; download tokens only need to cover the start of the request.
	replayUploadTokenTTL   = 30 * time.Minute
	replayDownloadTokenTTL = 15 * time.Minute
	// AzureStorageVersion is the x-ms-version the blob emulation answers with.
	AzureStorageVersion = "2017-11-09"
)

// ErrEmptyReplay is returned by Put when the upload carried no bytes.
var ErrEmptyReplay = errors.New("empty replay upload")

func trimReplayKey(key string) string {
	key = strings.TrimPrefix(key, "/")
	key = strings.TrimPrefix(key, "cloudfiles/")
	return strings.TrimPrefix(key, "/")
}

// IsMatchReplayKey reports whether key belongs to the replays namespace, valid
// or not; such keys are never looked up among the static cloud files.
func IsMatchReplayKey(key string) bool {
	return strings.HasPrefix(trimReplayKey(key), replayKeyPrefix)
}

// MatchReplayKey is the blob key of a match's blob: replays/<matchId>/<file>.
func MatchReplayKey(matchID int64, kind ReplayKind) string {
	return replayKeyPrefix + strconv.FormatInt(matchID, 10) + "/" + kind.Filename()
}

// ParseMatchReplayKey accepts only the keys MatchReplayKey produces, with or
// without a leading "/" or "/cloudfiles/".
func ParseMatchReplayKey(key string) (int64, ReplayKind, bool) {
	parts := strings.Split(trimReplayKey(key), "/")
	if len(parts) != 3 || parts[0]+"/" != replayKeyPrefix {
		return 0, 0, false
	}
	matchID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || matchID <= 0 || strconv.FormatInt(matchID, 10) != parts[1] {
		return 0, 0, false
	}
	for kind, name := range replayKindFiles {
		if parts[2] == name {
			return matchID, kind, true
		}
	}
	return 0, 0, false
}

// MatchReplaySAS is the shared-access query string the game appends to the
// blob URL, carrying the token as sig.
func MatchReplaySAS(title string, token string, expiry time.Time, write bool) string {
	permissions := "r"
	if write {
		permissions = "rcw"
	}
	return fmt.Sprintf("title=%s&sig=%s&se=%s&sv=%s&sp=%s&sr=b",
		url.QueryEscape(title), url.QueryEscape(token), url.QueryEscape(expiry.UTC().Format(time.RFC3339)),
		AzureStorageVersion, permissions)
}

// MatchReplaysFolder is the default per-game folder when Folder is 'auto':
// <resources>/userData/<gameId>/match-replays.
func MatchReplaysFolder(gameId string) string {
	return filepath.Join(paths.ResourcesDir, "userData", gameId, matchReplaysSubfolder)
}

// MatchReplayFile is the metadata of one stored blob.
type MatchReplayFile struct {
	MatchID int64
	Kind    ReplayKind
	Length  int64
	// MD5 is hex encoded. Files found at startup get it on their first download.
	MD5     string
	ModTime time.Time
	name    string
}

// MatchReplaysOpts configures one game's instance. A nil pointer passed to
// Initialize falls back to the runtime globals (the parsed config).
type MatchReplaysOpts struct {
	Enabled      bool
	Folder       string
	MaxFileSize  int64
	MaxTotalSize int64
	// Expiry 0 keeps files until MaxTotalSize evicts them.
	Expiry time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
}

func DefaultMatchReplaysOpts() *MatchReplaysOpts {
	return &MatchReplaysOpts{
		Enabled:      i.MatchReplaysEnabled,
		Folder:       i.MatchReplaysFolder,
		MaxFileSize:  i.MatchReplaysMaxFileSize,
		MaxTotalSize: i.MatchReplaysMaxTotalSize,
		Expiry:       i.MatchReplaysExpiry,
	}
}

type MatchReplays interface {
	Initialize(gameId string, opts *MatchReplaysOpts)
	Enabled() bool
	MaxFileSize() int64
	// Grant issues a token for one blob of one match. Write tokens also read.
	Grant(matchID int64, kind ReplayKind, write bool) (token string, expiry time.Time)
	// Authorized reports whether token was granted for that blob and access.
	Authorized(token string, matchID int64, kind ReplayKind, write bool) bool
	// Put stores body unless the blob already exists (stored is then false and
	// the body is discarded). The size limit is enforced while streaming.
	Put(matchID int64, kind ReplayKind, body io.Reader) (file *MatchReplayFile, stored bool, err error)
	Stat(matchID int64, kind ReplayKind) (*MatchReplayFile, bool)
	// Open returns the metadata and the file; the caller closes it.
	Open(matchID int64, kind ReplayKind) (*MatchReplayFile, *os.File, bool)
	TotalBytes() int64
	Close()
}

type replayID struct {
	matchID int64
	kind    ReplayKind
}

type replayGrant struct {
	replayID
	write  bool
	expiry time.Time
}

// MainMatchReplays is the disk-backed MatchReplays.
type MainMatchReplays struct {
	mu           sync.Mutex
	root         *os.Root
	folder       string
	files        map[replayID]*MatchReplayFile
	grants       map[string]replayGrant
	totalBytes   int64
	maxFileSize  int64
	maxTotalSize int64
	expiry       time.Duration
	now          func() time.Time
	enabled      bool
}

func (m *MainMatchReplays) Initialize(gameId string, opts *MatchReplaysOpts) {
	m.files = make(map[replayID]*MatchReplayFile)
	m.grants = make(map[string]replayGrant)
	m.now = time.Now
	if opts == nil {
		opts = DefaultMatchReplaysOpts()
	}
	if opts.Now != nil {
		m.now = opts.Now
	}
	m.maxFileSize = opts.MaxFileSize
	m.maxTotalSize = opts.MaxTotalSize
	m.expiry = opts.Expiry
	if !opts.Enabled {
		return
	}
	folder := MatchReplaysFolder(gameId)
	if base := strings.TrimSpace(opts.Folder); base != "" && !strings.EqualFold(base, autoFolder) {
		folder = filepath.Join(base, gameId)
	}
	if err := os.MkdirAll(folder, os.ModePerm); err != nil {
		logger.Warn("Match replays disabled for %s: cannot create %s: %s", gameId, folder, err)
		return
	}
	root, err := os.OpenRoot(folder)
	if err != nil {
		logger.Warn("Match replays disabled for %s: cannot open %s: %s", gameId, folder, err)
		return
	}
	m.folder = folder
	m.root = root
	m.enabled = true
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadLocked()
	m.expireLocked()
	m.evictLocked(replayID{})
}

func (m *MainMatchReplays) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled && m.root != nil
}

func (m *MainMatchReplays) MaxFileSize() int64 {
	return m.maxFileSize
}

func (m *MainMatchReplays) TotalBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalBytes
}

func replayFileName(id replayID) string {
	return strconv.FormatInt(id.matchID, 10) + "-" + id.kind.Filename()
}

func parseReplayFileName(name string) (replayID, bool) {
	idPart, file, ok := strings.Cut(name, "-")
	if !ok {
		return replayID{}, false
	}
	matchID, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || matchID <= 0 {
		return replayID{}, false
	}
	for kind, kindFile := range replayKindFiles {
		if file == kindFile {
			return replayID{matchID, kind}, true
		}
	}
	return replayID{}, false
}

// loadLocked indexes the blobs a previous run left and drops interrupted
// uploads. Requires m.mu.
func (m *MainMatchReplays) loadLocked() {
	entries, err := os.ReadDir(m.folder)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, partPrefix) && strings.HasSuffix(name, partSuffix) {
			m.removeName(name)
			continue
		}
		id, ok := parseReplayFileName(name)
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		m.files[id] = &MatchReplayFile{MatchID: id.matchID, Kind: id.kind, Length: info.Size(), ModTime: info.ModTime(), name: name}
		m.totalBytes += info.Size()
	}
}

func newReplayToken() string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (m *MainMatchReplays) Grant(matchID int64, kind ReplayKind, write bool) (string, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for token, g := range m.grants {
		if !now.Before(g.expiry) {
			delete(m.grants, token)
		}
	}
	ttl := replayDownloadTokenTTL
	if write {
		ttl = replayUploadTokenTTL
	}
	token := newReplayToken()
	expiry := now.Add(ttl)
	m.grants[token] = replayGrant{replayID: replayID{matchID, kind}, write: write, expiry: expiry}
	return token, expiry
}

func (m *MainMatchReplays) Authorized(token string, matchID int64, kind ReplayKind, write bool) bool {
	if token == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[token]
	if !ok || !m.now().Before(g.expiry) {
		return false
	}
	return g.replayID == replayID{matchID, kind} && (g.write || !write)
}

func (m *MainMatchReplays) Put(matchID int64, kind ReplayKind, body io.Reader) (*MatchReplayFile, bool, error) {
	id := replayID{matchID, kind}
	m.mu.Lock()
	if !m.enabled || m.root == nil {
		m.mu.Unlock()
		return nil, false, fs.ErrNotExist
	}
	if existing, ok := m.liveLocked(id); ok {
		cp := *existing
		m.mu.Unlock()
		_, _ = io.Copy(io.Discard, body)
		return &cp, false, nil
	}
	folder, maxFileSize := m.folder, m.maxFileSize
	m.mu.Unlock()

	tmp, err := os.CreateTemp(folder, partPrefix+"*"+partSuffix)
	if err != nil {
		return nil, false, err
	}
	tmpName := filepath.Base(tmp.Name())
	hash := md5.New()
	counter := &limitedWriter{limit: maxFileSize}
	_, err = io.Copy(io.MultiWriter(hash, counter, tmp), body)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil && counter.n == 0 {
		err = ErrEmptyReplay
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil || m.root == nil {
		if m.root != nil {
			m.removeName(tmpName)
		} else {
			_ = os.Remove(tmp.Name())
		}
		if err == nil {
			err = fs.ErrNotExist
		}
		return nil, false, err
	}
	if existing, ok := m.liveLocked(id); ok {
		// Another upload of the same blob finished first.
		m.removeName(tmpName)
		cp := *existing
		return &cp, false, nil
	}
	name := replayFileName(id)
	if err = m.root.Rename(tmpName, name); err != nil {
		m.removeName(tmpName)
		return nil, false, err
	}
	file := &MatchReplayFile{MatchID: matchID, Kind: kind, Length: counter.n, MD5: hex.EncodeToString(hash.Sum(nil)), ModTime: m.now(), name: name}
	if info, statErr := m.root.Stat(name); statErr == nil {
		file.ModTime = info.ModTime()
	}
	m.files[id] = file
	m.totalBytes += file.Length
	m.expireLocked()
	m.evictLocked(id)
	cp := *file
	return &cp, true, nil
}

func (m *MainMatchReplays) Stat(matchID int64, kind ReplayKind) (*MatchReplayFile, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled || m.root == nil {
		return nil, false
	}
	file, ok := m.liveLocked(replayID{matchID, kind})
	if !ok {
		return nil, false
	}
	cp := *file
	return &cp, true
}

func (m *MainMatchReplays) Open(matchID int64, kind ReplayKind) (*MatchReplayFile, *os.File, bool) {
	id := replayID{matchID, kind}
	m.mu.Lock()
	if !m.enabled || m.root == nil {
		m.mu.Unlock()
		return nil, nil, false
	}
	file, ok := m.liveLocked(id)
	if !ok {
		m.mu.Unlock()
		return nil, nil, false
	}
	f, err := m.root.Open(file.name)
	cp := *file
	m.mu.Unlock()
	if err != nil {
		return nil, nil, false
	}
	if cp.MD5 == "" {
		// Indexed at startup: hash once, on the first download.
		hash := md5.New()
		if _, err = io.Copy(hash, f); err != nil {
			_ = f.Close()
			return nil, nil, false
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, nil, false
		}
		cp.MD5 = hex.EncodeToString(hash.Sum(nil))
		m.mu.Lock()
		if current, ok := m.files[id]; ok && current == file {
			current.MD5 = cp.MD5
		}
		m.mu.Unlock()
	}
	return &cp, f, true
}

// Close releases the root. Files stay on disk and are indexed again next run.
func (m *MainMatchReplays) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.root != nil {
		_ = m.root.Close()
	}
	m.root = nil
	m.enabled = false
}

// liveLocked returns the indexed file unless its TTL has passed, in which case
// it is deleted. Requires m.mu.
func (m *MainMatchReplays) liveLocked(id replayID) (*MatchReplayFile, bool) {
	file, ok := m.files[id]
	if !ok {
		return nil, false
	}
	if m.expiredLocked(file) {
		m.deleteLocked(id, file)
		return nil, false
	}
	return file, true
}

func (m *MainMatchReplays) expiredLocked(file *MatchReplayFile) bool {
	return m.expiry > 0 && m.now().Sub(file.ModTime) >= m.expiry
}

func (m *MainMatchReplays) expireLocked() {
	for id, file := range m.files {
		if m.expiredLocked(file) {
			logger.Detail("Removed expired match replay %s", file.name)
			m.deleteLocked(id, file)
		}
	}
}

// evictLocked deletes the oldest blobs, never keep, until the total fits.
func (m *MainMatchReplays) evictLocked(keep replayID) {
	if m.maxTotalSize <= 0 {
		return
	}
	for m.totalBytes > m.maxTotalSize {
		var oldestID replayID
		var oldest *MatchReplayFile
		for id, file := range m.files {
			if id == keep {
				continue
			}
			if oldest == nil || file.ModTime.Before(oldest.ModTime) {
				oldestID, oldest = id, file
			}
		}
		if oldest == nil {
			return
		}
		logger.Detail("Evicting match replay %s to stay under the total size limit", oldest.name)
		m.deleteLocked(oldestID, oldest)
	}
}

func (m *MainMatchReplays) deleteLocked(id replayID, file *MatchReplayFile) {
	m.removeName(file.name)
	delete(m.files, id)
	m.totalBytes -= file.Length
}

// removeName deletes name from the root, retrying while Windows refuses to
// delete a file a download still holds open.
func (m *MainMatchReplays) removeName(name string) {
	var err error
	for attempt := range removeAttempts {
		if err = m.root.Remove(name); err == nil || errors.Is(err, fs.ErrNotExist) {
			return
		}
		if attempt < removeAttempts-1 {
			time.Sleep(removeRetryDelay)
		}
	}
	logger.Detail("Could not delete match replay %s: %s", name, err)
}
