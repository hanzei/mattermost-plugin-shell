package main

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// initRouter initializes the HTTP router for the plugin.
func (p *Plugin) initRouter() *mux.Router {
	router := mux.NewRouter()

	// Middleware to require that the user is logged in
	router.Use(p.MattermostAuthorizationRequired)

	apiRouter := router.PathPrefix("/api/v1").Subrouter()

	// Interactive host terminal (system-admin only; enforced in the handler).
	apiRouter.HandleFunc("/terminal", p.handleTerminal).Methods(http.MethodGet)

	// Lists shells available on the host plus the caller's current selection.
	apiRouter.HandleFunc("/shells", p.handleShells).Methods(http.MethodGet)

	// Lists the cluster nodes a terminal can run on.
	apiRouter.HandleFunc("/nodes", p.handleNodes).Methods(http.MethodGet)

	return router
}

// ServeHTTP routes plugin HTTP requests. The root URL is
// <siteUrl>/plugins/com.hanzei.shell/api/v1/.
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	p.router.ServeHTTP(w, r)
}

func (p *Plugin) MattermostAuthorizationRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("Mattermost-User-ID")
		if userID == "" {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
