package main

import (
	"net/http"
	"sync"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"

	"github.com/hanzei/mattermost-plugin-shell/server/command"
)

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	// client is the Mattermost server API client.
	client *pluginapi.Client

	// commandClient is the client used to register and execute slash commands.
	commandClient command.Command

	// router is the HTTP router for handling API requests.
	router *mux.Router

	// terminals tracks active interactive terminal sessions so they can be
	// torn down on deactivation.
	terminals *sessionManager

	// relay carries terminals whose browser socket and shell are on different
	// cluster nodes.
	relay *relay

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration
}

// OnActivate is invoked when the plugin is activated. If an error is returned, the plugin will be deactivated.
func (p *Plugin) OnActivate() error {
	p.client = pluginapi.NewClient(p.API, p.Driver)

	p.commandClient = command.NewCommandHandler(p.client)

	p.router = p.initRouter()

	p.terminals = newSessionManager()

	p.relay = newRelay()
	p.relay.publish = p.publishCluster
	p.relay.allowed = func(userID string) bool {
		return p.client.User.HasPermissionTo(userID, model.PermissionManageSystem)
	}
	p.relay.spawn = p.startShell
	p.relay.shells = p.localShells
	p.relay.logWarn = p.API.LogWarn
	p.relay.start()

	return nil
}

// OnDeactivate is invoked when the plugin is deactivated.
func (p *Plugin) OnDeactivate() error {
	if p.terminals != nil {
		p.terminals.closeAll()
	}
	if p.relay != nil {
		p.relay.closeAll()
	}
	return nil
}

// This will execute the commands that were registered in the NewCommandHandler function.
func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	response, err := p.commandClient.Handle(args)
	if err != nil {
		return nil, model.NewAppError("ExecuteCommand", "plugin.command.execute_command.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	return response, nil
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
