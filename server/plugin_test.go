package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
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
