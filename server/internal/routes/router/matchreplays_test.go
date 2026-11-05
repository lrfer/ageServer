package router

import "testing"

func TestMatchReplayUploadsHaveTheirOwnBodyCap(t *testing.T) {
	for path, raised := range map[string]bool{
		"/cloudfiles/replays/1/replay.rec.gz":       true,
		"/cloudfiles/Attrib.sga":                    false,
		"/cloudfiles/replays/../Attrib.sga":         false,
		"/cloudfiles/replays/1/../../../game/x/y/z": false,
	} {
		if got := raisedBodySizePath(path); got != raised {
			t.Fatalf("raisedBodySizePath(%q) = %v, want %v", path, got, raised)
		}
	}
}
