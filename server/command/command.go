package command

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/pluginapi"
)

type Handler struct {
	client *pluginapi.Client
}

type Command interface {
	Handle(args *model.CommandArgs) (*model.CommandResponse, error)
	executeHelloCommand(args *model.CommandArgs) *model.CommandResponse
	executeExecCommand(args *model.CommandArgs) *model.CommandResponse
}

const helloCommandTrigger = "hello"
const execCommandTrigger = "exec"

// execTimeout caps how long a command may run so a hung process doesn't block
// the plugin's RPC connection to the server.
const execTimeout = 30 * time.Second

// Register all your slash commands in the NewCommandHandler function.
func NewCommandHandler(client *pluginapi.Client) Command {
	err := client.SlashCommand.Register(&model.Command{
		Trigger:          helloCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Say hello to someone",
		AutoCompleteHint: "[@username]",
		AutocompleteData: model.NewAutocompleteData(helloCommandTrigger, "[@username]", "Username to say hello to"),
	})
	if err != nil {
		client.Log.Error("Failed to register command", "error", err)
	}

	err = client.SlashCommand.Register(&model.Command{
		Trigger:          execCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Run a shell command on the host machine",
		AutoCompleteHint: "[command]",
		AutocompleteData: model.NewAutocompleteData(execCommandTrigger, "[command]", "Shell command to run on the host"),
	})
	if err != nil {
		client.Log.Error("Failed to register command", "error", err)
	}

	return &Handler{
		client: client,
	}
}

// ExecuteCommand hook calls this method to execute the commands that were registered in the NewCommandHandler function.
func (c *Handler) Handle(args *model.CommandArgs) (*model.CommandResponse, error) {
	fields := strings.Fields(args.Command)
	if len(fields) == 0 {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Empty command",
		}, nil
	}
	trigger := strings.TrimPrefix(fields[0], "/")
	switch trigger {
	case helloCommandTrigger:
		return c.executeHelloCommand(args), nil
	case execCommandTrigger:
		return c.executeExecCommand(args), nil
	default:
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("Unknown command: %s", args.Command),
		}, nil
	}
}

func (c *Handler) executeHelloCommand(args *model.CommandArgs) *model.CommandResponse {
	if len(strings.Fields(args.Command)) < 2 {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Please specify a username",
		}
	}
	username := strings.Fields(args.Command)[1]
	return &model.CommandResponse{
		Text: "Hello, " + username,
	}
}

// executeExecCommand runs the rest of the slash command as a shell command on
// the host machine and returns the combined output.
func (c *Handler) executeExecCommand(args *model.CommandArgs) *model.CommandResponse {
	// Strip the leading "/exec " to get the raw command line. Using the raw
	// string (rather than re-joining Fields) preserves quoting and spacing.
	// Only system admins may run host commands.
	if !c.client.User.HasPermissionTo(args.UserId, model.PermissionManageSystem) {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         ":no_entry: This command is restricted to system administrators.",
		}
	}

	cmdLine := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args.Command), "/"+execCommandTrigger))
	if cmdLine == "" {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Please specify a command to run, e.g. `/exec uname -a`",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	// sh -c so that pipes, redirects, globs and env vars behave as expected.
	// Use an absolute path: the plugin process may run with an empty or minimal
	// $PATH, so a bare "sh" can fail to resolve ("executable file not found in
	// $PATH"). /bin/sh exists on Linux and macOS hosts.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdLine)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	runErr := cmd.Run()

	c.client.Log.Info("Executed shell command", "user_id", args.UserId, "command", cmdLine)

	output := combined.String()

	var status string
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		status = fmt.Sprintf(":hourglass: timed out after %s", execTimeout)
	case runErr != nil:
		status = fmt.Sprintf(":x: exited with error: %s", runErr.Error())
	default:
		status = ":white_check_mark: exited 0"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**`$ %s`** — %s\n", cmdLine, status)
	if strings.TrimSpace(output) == "" {
		b.WriteString("_(no output)_")
	} else {
		// Fenced code block keeps formatting and avoids markdown mangling.
		b.WriteString("```\n")
		b.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```")
	}

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         b.String(),
	}
}
