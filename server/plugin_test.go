package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeHTTPRequiresAuth verifies the router's auth middleware rejects a
// request that has no Mattermost-User-ID header before it reaches any handler.
func TestServeHTTPRequiresAuth(t *testing.T) {
	assert := assert.New(t)
	plugin := Plugin{}
	plugin.router = plugin.initRouter()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/shells", nil)

	plugin.ServeHTTP(nil, w, r)

	result := w.Result()
	assert.NotNil(result)
	defer func() { _ = result.Body.Close() }()
	assert.Equal(http.StatusUnauthorized, result.StatusCode)
}

func TestHandleNodes(t *testing.T) {
	setup := func(t *testing.T, admin bool) *Plugin {
		api := &plugintest.API{}
		t.Cleanup(func() { api.AssertExpectations(t) })
		api.On("HasPermissionTo", "user1", model.PermissionManageSystem).Return(admin)
		if admin {
			api.On("GetClusterNodes").Return([]*model.ClusterInfo{
				{Id: "n1", Hostname: "10.0.0.1", IPAddress: "10.0.0.1:8074", ConfigHash: "h"},
				{Id: "n2", Hostname: "10.0.0.2", IPAddress: "10.0.0.2:8074", ConfigHash: "h"},
			}, nil)
		}
		p := &Plugin{}
		p.SetAPI(api)
		p.client = pluginapi.NewClient(api, &plugintest.Driver{})
		p.router = p.initRouter()
		return p
	}
	request := func(p *Plugin) *http.Response {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
		r.Header.Set("Mattermost-User-ID", "user1")
		p.ServeHTTP(nil, w, r)
		return w.Result()
	}

	t.Run("lists nodes for admins", func(t *testing.T) {
		resp := request(setup(t, true))
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.JSONEq(t, `[
			{"id": "n1", "hostname": "10.0.0.1", "ipaddress": "10.0.0.1:8074"},
			{"id": "n2", "hostname": "10.0.0.2", "ipaddress": "10.0.0.2:8074"}
		]`, string(body))
	})

	t.Run("forbidden for others", func(t *testing.T) {
		resp := request(setup(t, false))
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}
