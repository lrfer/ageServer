package models

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type replayClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *replayClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *replayClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestMatchReplays(t *testing.T, folder string, opts MatchReplaysOpts) *MainMatchReplays {
	t.Helper()
	opts.Enabled = true
	opts.Folder = folder
	if opts.MaxFileSize == 0 {
		opts.MaxFileSize = 1 << 20
	}
	if opts.MaxTotalSize == 0 {
		opts.MaxTotalSize = 4 << 20
	}
	store := &MainMatchReplays{}
	store.Initialize("age4", &opts)
	if !store.Enabled() {
		t.Fatal("store not enabled")
	}
	t.Cleanup(store.Close)
	return store
}

func replayDiskFiles(t *testing.T, folder string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(folder, "age4"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestParseMatchReplayKey(t *testing.T) {
	for _, key := range []string{"replays/42/replay.rec.gz", "/replays/42/replay.rec.gz", "/cloudfiles/replays/42/stats.rgs.gz"} {
		if id, _, ok := ParseMatchReplayKey(key); !ok || id != 42 {
			t.Fatalf("%q: id=%d ok=%v", key, id, ok)
		}
	}
	for _, key := range []string{
		"replays/../x", "replays/42/../replay.rec.gz", "replays/042/replay.rec.gz", "replays/-1/replay.rec.gz",
		"replays/x/replay.rec.gz", "replays/42/other.bin", "replays/42/replay.rec.gz/extra", "other/42/replay.rec.gz",
	} {
		if _, _, ok := ParseMatchReplayKey(key); ok {
			t.Fatalf("%q accepted", key)
		}
	}
	if !IsMatchReplayKey("/cloudfiles/replays/../x") || IsMatchReplayKey("Attrib.sga") {
		t.Fatal("IsMatchReplayKey")
	}
	if sas := MatchReplaySAS("age4", "tok", time.Unix(0, 0), true); !strings.Contains(sas, "sp=rcw") || !strings.Contains(sas, "sig=tok") {
		t.Fatalf("write SAS %q", sas)
	}
	if sas := MatchReplaySAS("age4", "tok", time.Unix(0, 0), false); !strings.Contains(sas, "sp=r&") {
		t.Fatalf("read SAS %q", sas)
	}
}

func TestMatchReplayPutStoresOnDiskAndFirstUploadWins(t *testing.T) {
	folder := t.TempDir()
	store := newTestMatchReplays(t, folder, MatchReplaysOpts{})

	first := []byte("first replay")
	file, stored, err := store.Put(7, ReplayKindReplay, bytes.NewReader(first))
	sum := md5.Sum(first)
	if err != nil || !stored || file.Length != int64(len(first)) || file.MD5 != hex.EncodeToString(sum[:]) {
		t.Fatalf("Put: %+v stored=%v err=%v", file, stored, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(folder, "age4", "7-replay.rec.gz"))
	if err != nil || !bytes.Equal(onDisk, first) {
		t.Fatalf("on disk %q (%v)", onDisk, err)
	}

	if _, stored, err = store.Put(7, ReplayKindReplay, strings.NewReader("second")); err != nil || stored {
		t.Fatalf("second Put stored=%v err=%v", stored, err)
	}
	if onDisk, _ = os.ReadFile(filepath.Join(folder, "age4", "7-replay.rec.gz")); !bytes.Equal(onDisk, first) {
		t.Fatalf("second upload replaced the first: %q", onDisk)
	}
	if names := replayDiskFiles(t, folder); len(names) != 1 {
		t.Fatalf("leftover files: %v", names)
	}
}

func TestMatchReplayPutRejectsOversizedAndEmpty(t *testing.T) {
	folder := t.TempDir()
	store := newTestMatchReplays(t, folder, MatchReplaysOpts{MaxFileSize: 10, MaxTotalSize: 100})

	if _, _, err := store.Put(1, ReplayKindReplay, bytes.NewReader(make([]byte, 11))); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	if _, _, err := store.Put(1, ReplayKindReplay, bytes.NewReader(nil)); !errors.Is(err, ErrEmptyReplay) {
		t.Fatalf("empty: %v", err)
	}
	if names := replayDiskFiles(t, folder); len(names) != 0 {
		t.Fatalf("rejected uploads left files: %v", names)
	}
	if _, ok := store.Stat(1, ReplayKindReplay); ok || store.TotalBytes() != 0 {
		t.Fatal("rejected upload was indexed")
	}
}

func TestMatchReplayQuotaEvictsOldest(t *testing.T) {
	folder := t.TempDir()
	clock := &replayClock{now: time.Now()}
	store := newTestMatchReplays(t, folder, MatchReplaysOpts{MaxFileSize: 10, MaxTotalSize: 25, Now: clock.Now})

	for matchID := int64(1); matchID <= 3; matchID++ {
		if _, _, err := store.Put(matchID, ReplayKindReplay, bytes.NewReader(make([]byte, 10))); err != nil {
			t.Fatal(err)
		}
		// Distinct modification times so "oldest" is well defined.
		name := filepath.Join(folder, "age4", replayFileName(replayID{matchID, ReplayKindReplay}))
		stamp := time.Now().Add(time.Duration(matchID) * time.Minute)
		_ = os.Chtimes(name, stamp, stamp)
		store.mu.Lock()
		store.files[replayID{matchID, ReplayKindReplay}].ModTime = stamp
		store.mu.Unlock()
	}
	if store.TotalBytes() > 25 {
		t.Fatalf("total %d over the quota", store.TotalBytes())
	}
	if _, ok := store.Stat(1, ReplayKindReplay); ok {
		t.Fatal("oldest replay was not evicted")
	}
	for _, matchID := range []int64{2, 3} {
		if _, ok := store.Stat(matchID, ReplayKindReplay); !ok {
			t.Fatalf("replay %d evicted", matchID)
		}
	}
	if names := replayDiskFiles(t, folder); len(names) != 2 {
		t.Fatalf("on disk: %v", names)
	}
}

func TestMatchReplaysSurviveRestartAndExpire(t *testing.T) {
	folder := t.TempDir()
	clock := &replayClock{now: time.Now()}
	opts := MatchReplaysOpts{Expiry: time.Hour, Now: clock.Now}
	store := newTestMatchReplays(t, folder, opts)
	payload := []byte("kept across restarts")
	if _, _, err := store.Put(9, ReplayKindStats, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	// An interrupted upload from the previous run.
	if err := os.WriteFile(filepath.Join(folder, "age4", partPrefix+"x"+partSuffix), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	store.Close()

	restarted := newTestMatchReplays(t, folder, opts)
	file, f, ok := restarted.Open(9, ReplayKindStats)
	if !ok {
		t.Fatal("replay lost on restart")
	}
	_ = f.Close()
	sum := md5.Sum(payload)
	if file.Length != int64(len(payload)) || file.MD5 != hex.EncodeToString(sum[:]) {
		t.Fatalf("reindexed %+v", file)
	}
	if names := replayDiskFiles(t, folder); len(names) != 1 {
		t.Fatalf("partial upload not purged: %v", names)
	}

	clock.Advance(2 * time.Hour)
	if _, ok := restarted.Stat(9, ReplayKindStats); ok {
		t.Fatal("expired replay still served")
	}
	if names := replayDiskFiles(t, folder); len(names) != 0 {
		t.Fatalf("expired replay left on disk: %v", names)
	}
}

func TestMatchReplayGrantsAreBoundToBlobAndPermission(t *testing.T) {
	clock := &replayClock{now: time.Now()}
	store := newTestMatchReplays(t, t.TempDir(), MatchReplaysOpts{Now: clock.Now})

	read, _ := store.Grant(5, ReplayKindReplay, false)
	write, _ := store.Grant(5, ReplayKindReplay, true)
	for _, tc := range []struct {
		token string
		match int64
		kind  ReplayKind
		write bool
		want  bool
	}{
		{read, 5, ReplayKindReplay, false, true},
		{read, 5, ReplayKindReplay, true, false}, // read token cannot write
		{write, 5, ReplayKindReplay, true, true},
		{write, 5, ReplayKindReplay, false, true},
		{write, 6, ReplayKindReplay, true, false}, // other match
		{write, 5, ReplayKindStats, true, false},  // other blob
		{"", 5, ReplayKindReplay, false, false},
		{"forged", 5, ReplayKindReplay, false, false},
	} {
		if got := store.Authorized(tc.token, tc.match, tc.kind, tc.write); got != tc.want {
			t.Fatalf("Authorized(%q, %d, %d, write=%v) = %v", tc.token, tc.match, tc.kind, tc.write, got)
		}
	}
	clock.Advance(replayUploadTokenTTL + time.Second)
	if store.Authorized(write, 5, ReplayKindReplay, true) {
		t.Fatal("expired token accepted")
	}
}

func TestMatchReplaysDisabledStoresNothing(t *testing.T) {
	store := &MainMatchReplays{}
	store.Initialize("age4", &MatchReplaysOpts{Enabled: false, Folder: t.TempDir()})
	if store.Enabled() {
		t.Fatal("enabled")
	}
	if _, _, err := store.Put(1, ReplayKindReplay, strings.NewReader("x")); err == nil {
		t.Fatal("disabled store accepted an upload")
	}
}
