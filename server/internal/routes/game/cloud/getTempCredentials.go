package cloud

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	i "github.com/luskaner/ageServer/server/internal"
	"github.com/luskaner/ageServer/server/internal/models"
)

func GetTempCredentials(w http.ResponseWriter, r *http.Request) {
	fullKey := r.URL.Query().Get("key")
	key := strings.TrimPrefix(fullKey, "/cloudfiles/")
	key = strings.TrimPrefix(key, "/")
	game := models.G(r)
	if models.IsMatchReplayKey(key) {
		matchReplayCredentials(w, game, fullKey, key)
		return
	}
	cloudfiles := game.Resources().CloudFiles()
	if cloudfiles.Credentials == nil {
		i.JSON(&w, i.A{2, 0, "", fullKey})
		return
	}
	cred := models.CreateCredential(cloudfiles.Credentials, &key)
	t := cred.Expiry()
	tUnix := t.Unix()
	for _, file := range cloudfiles.Value {
		if file.Key == key {
			se := url.QueryEscape(t.Format(time.RFC3339))
			sv := url.QueryEscape(file.Version)
			i.JSON(&w, i.A{0, tUnix, fmt.Sprintf("title=%s&sig=%s&se=%s&sv=%s&sp=r&sr=b", game.Title(), url.QueryEscape(cred.Id()), se, sv), fullKey})
			return
		}
	}
	i.JSON(&w, i.A{2, tUnix, "", fullKey})
}

// matchReplayCredentials issues a read token for a stored match replay. Keys
// other than the exact ones this server hands out are never found.
func matchReplayCredentials(w http.ResponseWriter, game models.Game, fullKey string, key string) {
	store := game.MatchReplays()
	matchID, kind, ok := models.ParseMatchReplayKey(key)
	if !ok || store == nil || !store.Enabled() {
		i.JSON(&w, i.A{2, 0, "", fullKey})
		return
	}
	if _, exists := store.Stat(matchID, kind); !exists {
		i.JSON(&w, i.A{2, 0, "", fullKey})
		return
	}
	token, expiry := store.Grant(matchID, kind, false)
	i.JSON(&w, i.A{0, expiry.Unix(), models.MatchReplaySAS(game.Title(), token, expiry, false), "/cloudfiles/" + models.MatchReplayKey(matchID, kind)})
}
