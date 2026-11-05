package router

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"

	"github.com/luskaner/ageServer/common"
	"github.com/luskaner/ageServer/common/game"
	"github.com/luskaner/ageServer/server/internal/routes/apiAgeOfEmpires/mods"
	"github.com/luskaner/ageServer/server/internal/routes/apiAgeOfEmpires/textmoderation"
	"github.com/luskaner/ageServer/server/internal/routes/matchcontent"
)

type ApiAgeOfEmpires struct {
	Proxy
	apiName string
	// extraRoutes registers the routes only this host serves. upstream is the
	// proxy to the official host, nil without Internet.
	extraRoutes func(group *Group, upstream *httputil.ReverseProxy)
}

func (a *ApiAgeOfEmpires) Name() string {
	return a.apiName
}

func (a *ApiAgeOfEmpires) Initialize(_ string) bool {
	return true
}

func (a *ApiAgeOfEmpires) InitializeRoutes(gameId string, next http.Handler) http.Handler {
	a.Proxy = NewProxy(a.Name(), func(_ string, _ http.Handler) http.Handler {
		a.group.HandleFunc("POST", "/textmoderation", textmoderation.TextModeration)
		a.group.HandleFunc("POST", "/api/MatchContent/", matchcontent.Upload)
		a.group.HandleFunc("GET", "/api/MatchContent/{matchId}", matchcontent.Download)
		a.group.HandleFunc("GET", "/api/MatchContent/{matchId}/file/{hash}", matchcontent.DownloadFile)
		if a.extraRoutes != nil {
			a.extraRoutes(a.group, a.Proxy.proxy)
		}
		return a.group.mux
	})
	return a.Proxy.InitializeRoutes(gameId, next)
}

func NewApiAgeOfEmpires() *ApiAgeOfEmpires {
	return &ApiAgeOfEmpires{
		apiName: common.ApiAgeOfEmpires,
	}
}

type Aoe4ApiAgeOfEmpires struct {
	ApiAgeOfEmpires
}

func (a *Aoe4ApiAgeOfEmpires) Initialize(gameId string) bool {
	return gameId == game.AoE4
}

func NewAoe4ApiAgeOfEmpires() *Aoe4ApiAgeOfEmpires {
	return &Aoe4ApiAgeOfEmpires{
		ApiAgeOfEmpires{
			apiName:     common.Aoe4ApiAgeOfEmpires,
			extraRoutes: registerLocalModsRoutes,
		},
	}
}

// registerLocalModsRoutes wires the AoE IV mod browser. Without an upstream
// (no Internet, or the official host does not resolve) it is answered locally.
// With one, each request goes to the official service first, so the official
// launcher keeps subscribing to online mods; only a client the service turns
// down, or a service that cannot be reached, gets the local mods.
func registerLocalModsRoutes(group *Group, upstream *httputil.ReverseProxy) {
	for _, rt := range mods.Routes() {
		handler := rt.Handler
		if upstream != nil && !rt.LocalOnly {
			handler = officialFirst(upstream, handler)
		}
		group.HandleFunc(rt.Method, rt.Path, handler)
	}
}

// maxOfficialFirstBody bounds the body buffered to replay it locally; the mod
// browser requests are under 1 KiB.
const maxOfficialFirstBody = 64 << 10

// errOfficialRejected marks a 401 from the official service.
var errOfficialRejected = errors.New("official service rejected the client")

// officialFirst proxies the request to the official service and relays its
// answer, except when it responds 401 or the proxying fails: then the request
// (with its body replayed) is answered by local instead.
func officialFirst(upstream *httputil.ReverseProxy, local http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			var err error
			body, err = io.ReadAll(io.LimitReader(r.Body, maxOfficialFirstBody+1))
			_ = r.Body.Close()
			if err != nil || len(body) > maxOfficialFirstBody {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
		}
		replay := func() *http.Request {
			c := r.Clone(r.Context())
			c.Body = io.NopCloser(bytes.NewReader(body))
			c.ContentLength = int64(len(body))
			return c
		}

		proxy := *upstream
		proxy.ModifyResponse = func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				return errOfficialRejected
			}
			return nil
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
			local(w, replay())
		}
		proxy.ServeHTTP(w, replay())
	}
}
