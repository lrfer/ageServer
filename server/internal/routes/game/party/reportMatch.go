package party

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	i "github.com/luskaner/ageServer/server/internal"
	"github.com/luskaner/ageServer/server/internal/logger"
	"github.com/luskaner/ageServer/server/internal/models"
)

// reportMatchRequest mirrors the form the client posts. The list fields travel
// as JSON arrays, which i.Json decodes transparently.
type reportMatchRequest struct {
	MatchID      int64            `schema:"match_id"`
	ProfileIDs   i.Json[[]int32]  `schema:"profile_ids"`
	Results      i.Json[[]int32]  `schema:"results"`
	TeamIDs      i.Json[[]int32]  `schema:"teamIDs"`
	RaceIDs      i.Json[[]int32]  `schema:"race_ids"`
	XPGained     i.Json[[]int32]  `schema:"xpGained"`
	CountersZip  string           `schema:"countersZip"`
	ItemUpdates  i.Json[[]string] `schema:"itemUpdates"`
	CheckSums    i.Json[[]int32]  `schema:"checkSums"`
	SimPlayerIDs i.Json[[]int32]  `schema:"simplayerIDs"`
}

// reportMatchResponse is the payload the AoE2:DE client expects from
// /game/party/reportMatch. It has been verified against a real 1.16.0 match, so
// keep it byte-for-byte compatible.
func reportMatchResponse() i.A {
	return i.A{2, i.A{}, i.A{-1, "", "", "", 0, ""}, i.A{-1, "", "", "", 0, ""}, nil, i.A{}, i.A{}, i.A{}, i.A{}, i.A{}, i.A{}, 0, nil, i.A{}, i.A{}, i.A{}}
}

func decodeCountersZip(raw string, playerCount int) []string {
	cleaned := strings.Trim(raw, "\" \t\r\n")
	if cleaned == "" {
		return nil
	}
	compressed, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil
	}
	defer func() { _ = zr.Close() }()
	decompressed, err := io.ReadAll(io.LimitReader(zr, 1<<20))
	if err != nil {
		return nil
	}
	var perPlayer []string
	if err := json.Unmarshal(decompressed, &perPlayer); err == nil {
		return perPlayer
	}
	trimmed := strings.TrimSpace(string(decompressed))
	if strings.HasPrefix(trimmed, "{") && playerCount > 0 {
		out := make([]string, playerCount)
		for idx := range out {
			out[idx] = trimmed
		}
		return out
	}
	return nil
}

func isInProgressMatchReport(results []int32) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r != 3 {
			return false
		}
	}
	return true
}

func ReportMatch(w http.ResponseWriter, r *http.Request) {
	var req reportMatchRequest
	if err := i.Bind(r, &req); err != nil {
		logger.Printf("party.reportMatch: could not bind form: %s\n", err)
		i.JSON(&w, reportMatchResponse())
		return
	}
	if req.MatchID == 0 {
		logger.Printf("party.reportMatch: missing or invalid match_id\n")
		i.JSON(&w, reportMatchResponse())
		return
	}

	g := models.G(r)
	now := time.Now()
	inProgress := isInProgressMatchReport(req.Results.Data)
	counters := decodeCountersZip(req.CountersZip, len(req.ProfileIDs.Data))

	result := models.MatchResult{
		MatchID:       req.MatchID,
		Players:       req.ProfileIDs.Data,
		Results:       req.Results.Data,
		Teams:         req.TeamIDs.Data,
		Races:         req.RaceIDs.Data,
		XPGained:      req.XPGained.Data,
		Counters:      counters,
		CustomUpdates: req.ItemUpdates.Data,
		WinnerTeam:    -1,
		FinishedAt:    now,
	}

	if advs := g.Advertisements(); advs != nil {
		if adv, ok := advs.GetAdvertisement(int32(req.MatchID)); ok && adv != nil {
			advs.WithReadLock(int32(req.MatchID), func() {
				result.MapName = adv.UnsafeGetMapName()
				result.MaxPlayers = int32(adv.UnsafeGetMaxPlayers())
				result.MatchTypeID = int32(adv.UnsafeGetMatchType())
				result.EncodedOptions = adv.UnsafeGetOptions()
				result.EncodedSlots = adv.UnsafeGetSlotInfo()
				result.Description = adv.UnsafeGetDescription()
				result.ModName = adv.UnsafeGetModName()
				result.ModVersion = adv.UnsafeGetModVersion()
				result.ModDllFile = adv.UnsafeGetModDllFile()
				result.ModDllCRC = adv.UnsafeGetModDllChecksum()
				result.DataCRC = adv.UnsafeGetDataChecksum()
				result.AppBinaryCRC = adv.UnsafeGetAppBinaryChecksum()
				result.VersionFlags = int32(adv.UnsafeGetVersionFlags())
				if st := adv.UnsafeGetStartTime(); st > 0 {
					result.StartedAt = time.Unix(st, 0)
				}
			})
		}
	}

	if len(result.Players) == 0 {
		logger.Printf("party.reportMatch: match=%d has no profile_ids\n", req.MatchID)
		i.JSON(&w, reportMatchResponse())
		return
	}

	history := g.MatchHistory()
	if history == nil {
		logger.Printf("party.reportMatch: match=%d no match history store\n", req.MatchID)
		i.JSON(&w, reportMatchResponse())
		return
	}
	// Checked before ApplyResult inserts the row: only a lobby hosted on this
	// server (recorded when it was created) may get its replay stored here.
	hostedHere := history.MatchExists(req.MatchID)
	ratings, rated := history.ApplyResult(result)

	users := g.Users()
	var updates []string
	for _, profile := range result.Players {
		if rating, ok := ratings[profile]; ok && rating != nil {
			// Refresh the cached rating so the next profile encode shows the
			// new value without waiting for another database read.
			if user, found := users.GetUserById(profile); found {
				user.SetRating(rating.Rating)
			}
			updates = append(updates, strconv.FormatInt(int64(profile), 10)+"="+strconv.Itoa(int(rating.Rating)))
		}
	}
	logger.Printf(
		"party.reportMatch: stored match=%d profiles=%v results=%v teams=%v races=%v rated=%t ratings=[%s]\n",
		req.MatchID,
		result.Players,
		result.Results,
		result.Teams,
		result.Races,
		rated,
		strings.Join(updates, " "),
	)

	resp := reportMatchResponse()
	if !inProgress && hostedHere {
		addReplayUploadTargets(resp, r, g, req.MatchID, result.Players)
	}
	i.JSON(&w, resp)
}

// addReplayUploadTargets tells the reporting player where to upload the match
// replay and stats: this same server (r.Host is the game API host, which the
// hosts redirection already points here), never the official storage. Only a
// player of the match gets them, and only for blobs not stored yet.
func addReplayUploadTargets(resp i.A, r *http.Request, g models.Game, matchID int64, players []int32) {
	store := g.MatchReplays()
	if store == nil || !store.Enabled() {
		return
	}
	sess, ok := models.SessionFromRequest(r)
	if !ok || sess == nil || !slices.Contains(players, sess.GetUserId()) {
		return
	}
	targets := map[models.ReplayKind]struct {
		slot int
		kind string
	}{
		models.ReplayKindReplay: {2, "UploadCloudMatchReplay-REPLAY"},
		models.ReplayKindStats:  {3, "UploadCloudMatchReplay-DATA"},
	}
	for kind, target := range targets {
		if _, stored := store.Stat(matchID, kind); stored {
			continue
		}
		token, expiry := store.Grant(matchID, kind, true)
		resp[0] = 0
		resp[target.slot] = i.A{
			sess.GetUserId(),
			r.Host,
			"/cloudfiles/" + models.MatchReplayKey(matchID, kind),
			models.MatchReplaySAS(g.Title(), token, expiry, true),
			expiry.Unix(),
			target.kind,
		}
	}
}
