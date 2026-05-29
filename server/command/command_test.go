package command

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type env struct {
	client *pluginapi.Client
	api    *plugintest.API
}

func setupTest() *env {
	api := &plugintest.API{}
	driver := &plugintest.Driver{}
	client := pluginapi.NewClient(api, driver)

	return &env{
		client: client,
		api:    api,
	}
}

func TestInspectCommand(t *testing.T) {
	assert := assert.New(t)
	env := setupTest()

	env.api.On("RegisterCommand", mock.AnythingOfType("*model.Command")).Return(nil)
	env.api.On("HasPermissionTo", "uid", model.PermissionManageSystem).Return(true)
	env.api.On("LogInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	cmdHandler := NewCommandHandler(env.client)

	// inspect runs entirely in Go and must work without spawning a shell.
	args := &model.CommandArgs{
		UserId:  "uid",
		Command: "/inspect info",
	}
	response, err := cmdHandler.Handle(args)
	assert.Nil(err)
	assert.Contains(response.Text, "Container / runtime info")
	assert.Contains(response.Text, "GOOS/GOARCH")

	// Non-admins are rejected.
	env2 := setupTest()
	env2.api.On("RegisterCommand", mock.AnythingOfType("*model.Command")).Return(nil)
	env2.api.On("HasPermissionTo", "nobody", model.PermissionManageSystem).Return(false)
	handler2 := NewCommandHandler(env2.client)
	resp2, err := handler2.Handle(&model.CommandArgs{UserId: "nobody", Command: "/inspect env"})
	assert.Nil(err)
	assert.Contains(resp2.Text, "restricted to system administrators")
}
