package cloud

import (
	"net/http"
	"strings"

	i "github.com/luskaner/ageServer/server/internal"
	"github.com/luskaner/ageServer/server/internal/models"
)

// GetTempUploadCredentials issues a write token for one blob of a match replay,
// to be uploaded to this server (never to the official storage). Only a player
// of a match reported here gets one, and only for that match's own blobs.
func GetTempUploadCredentials(w http.ResponseWriter, r *http.Request) {
	fullKey := r.URL.Query().Get("key")
	if fullKey == "" {
		fullKey = r.FormValue("key")
	}
	key := strings.TrimPrefix(strings.TrimPrefix(fullKey, "/cloudfiles/"), "/")
	game := models.G(r)
	store := game.MatchReplays()
	history := game.MatchHistory()
	matchID, kind, ok := models.ParseMatchReplayKey(key)
	sess, hasSession := models.SessionFromRequest(r)
	if !ok || store == nil || !store.Enabled() || history == nil || !hasSession || sess == nil ||
		!history.IsMatchPlayer(matchID, sess.GetUserId()) {
		i.JSON(&w, i.A{2, "", "", "", 0})
		return
	}
	token, expiry := store.Grant(matchID, kind, true)
	i.JSON(&w, i.A{0, r.Host, "/cloudfiles/" + models.MatchReplayKey(matchID, kind), models.MatchReplaySAS(game.Title(), token, expiry, true), expiry.Unix()})
}
