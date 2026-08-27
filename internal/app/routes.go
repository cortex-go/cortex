package app

import "net/http"

type routeBoundary string

const (
	boundaryPublic  routeBoundary = "public"
	boundarySession routeBoundary = "session"
)

type routePolicy struct {
	Path     string
	Boundary routeBoundary
	Methods  []string
}

type registeredRoute struct {
	Policy  routePolicy
	Handler http.HandlerFunc
}

func (a *App) apiRoutes() []registeredRoute {
	public := func(path string, methods []string, handler http.HandlerFunc) registeredRoute {
		return registeredRoute{routePolicy{Path: path, Boundary: boundaryPublic, Methods: methods}, handler}
	}
	session := func(path string, methods []string, handler http.HandlerFunc) registeredRoute {
		return registeredRoute{routePolicy{Path: path, Boundary: boundarySession, Methods: methods}, handler}
	}
	get := []string{http.MethodGet}
	post := []string{http.MethodPost}
	return []registeredRoute{
		public("/api/auth/state", get, a.authState),
		public("/api/auth/setup", post, a.authSetup),
		public("/api/auth/login", post, a.authLogin),
		session("/api/auth/logout", post, a.authLogout),
		session("/api/auth/password", post, a.authPassword),
		session("/api/auth/totp/begin", post, a.authTOTPBegin),
		session("/api/auth/totp/enable", post, a.authTOTPEnable),
		session("/api/auth/totp/disable", post, a.authTOTPDisable),
		session("/api/auth/google", post, a.authGoogleConfig),
		public("/api/auth/google/start", get, a.googleStart),
		public("/api/auth/google/callback", get, a.googleCallback),
		session("/api/status", get, a.status),
		session("/api/settings", []string{http.MethodGet, http.MethodPost}, a.settingsAPI),
		session("/api/files", get, a.filesAPI),
		session("/api/file", get, a.fileAPI),
		session("/api/agent/status", get, a.agentStatus),
		session("/api/agent/run", post, a.agentRun),
		session("/api/conversations", []string{http.MethodGet, http.MethodPost}, a.conversationsAPI),
		session("/api/conversation", []string{http.MethodPut, http.MethodDelete}, a.conversationAPI),
	}
}

func (a *App) publicAPI(path string) bool {
	for _, route := range a.apiRoutes() {
		if route.Policy.Path == path {
			return route.Policy.Boundary == boundaryPublic
		}
	}
	return false
}
