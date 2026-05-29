package command

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
	executeInspectCommand(args *model.CommandArgs) *model.CommandResponse
}

const (
	helloCommandTrigger   = "hello"
	execCommandTrigger    = "exec"
	inspectCommandTrigger = "inspect"
)

// inspectBinDirs are scanned to discover available executables. We use a fixed
// list rather than $PATH on purpose: a minimal container often has an empty or
// near-empty $PATH (the very condition that makes /exec fail), so walking $PATH
// would report nothing precisely when discovery matters most.
var inspectBinDirs = []string{"/bin", "/usr/bin", "/sbin", "/usr/sbin", "/usr/local/bin"}

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

	err = client.SlashCommand.Register(&model.Command{
		Trigger:          inspectCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Inspect the plugin's container without a shell",
		AutoCompleteHint: "[env|bins|info|cat <path>]",
		AutocompleteData: model.NewAutocompleteData(inspectCommandTrigger, "[env|bins|info|cat <path>]", "Introspect the container using pure Go (no shell required)"),
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
	case inspectCommandTrigger:
		return c.executeInspectCommand(args), nil
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
	// #nosec G204 -- running an arbitrary host command is the whole point of
	// /exec; access is gated to system admins above.
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

// executeInspectCommand reports facts about the container the plugin runs in
// using only the Go standard library. Unlike /exec it never spawns a process,
// so it works in shell-less images (distroless, scratch) where /bin/sh and even
// utilities like env or ls are absent.
func (c *Handler) executeInspectCommand(args *model.CommandArgs) *model.CommandResponse {
	if !c.client.User.HasPermissionTo(args.UserId, model.PermissionManageSystem) {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         ":no_entry: This command is restricted to system administrators.",
		}
	}

	fields := strings.Fields(args.Command)
	sub := "info"
	if len(fields) >= 2 {
		sub = fields[1]
	}

	var out string
	switch sub {
	case "env":
		out = inspectEnv()
	case "bins":
		out = inspectBins()
	case "info":
		out = inspectInfo()
	case "cat":
		if len(fields) < 3 {
			out = "Usage: `/inspect cat <path>` (e.g. `/inspect cat /etc/os-release` or `/inspect cat /proc/1/environ`)"
			break
		}
		out = inspectCat(fields[2])
	default:
		out = fmt.Sprintf("Unknown subcommand %q. Try `env`, `bins`, `info`, or `cat <path>`.", sub)
	}

	c.client.Log.Info("Inspected container", "user_id", args.UserId, "subcommand", sub)

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         out,
	}
}

// inspectEnv returns the process environment. Because the plugin runs as a
// subprocess of the Mattermost server, this is the server container's
// environment. (For PID 1's environment specifically, use `cat /proc/1/environ`.)
func inspectEnv() string {
	env := os.Environ()
	sort.Strings(env)
	if len(env) == 0 {
		return "_(empty environment)_"
	}
	return "**Environment** (" + fmt.Sprintf("%d", len(env)) + " vars)\n```\n" + strings.Join(env, "\n") + "\n```"
}

// inspectBins lists executables found in a fixed set of common bin directories.
func inspectBins() string {
	var b strings.Builder
	b.WriteString("**Executables in common bin directories**\n")
	found := false
	for _, dir := range inspectBinDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(&b, "\n`%s` — _%s_\n", dir, err)
			continue
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "\n`%s` (%d):\n", dir, len(names))
		if len(names) == 0 {
			b.WriteString("_(empty)_\n")
			continue
		}
		found = true
		b.WriteString("```\n")
		b.WriteString(strings.Join(names, " "))
		b.WriteString("\n```\n")
	}
	if !found {
		b.WriteString("\n_No executables found — this looks like a scratch/distroless image. Use `/inspect cat <path>` and `/inspect env` to introspect further._")
	}
	return b.String()
}

// inspectInfo reports Go runtime and basic OS facts about the container.
func inspectInfo() string {
	var b strings.Builder
	b.WriteString("**Container / runtime info**\n```\n")
	hostname, _ := os.Hostname()
	cwd, _ := os.Getwd()
	exe, _ := os.Executable()
	fmt.Fprintf(&b, "go version : %s\n", runtime.Version())
	fmt.Fprintf(&b, "GOOS/GOARCH: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "NumCPU     : %d\n", runtime.NumCPU())
	fmt.Fprintf(&b, "hostname   : %s\n", hostname)
	fmt.Fprintf(&b, "uid/gid    : %d/%d\n", os.Getuid(), os.Getgid())
	fmt.Fprintf(&b, "pid        : %d\n", os.Getpid())
	fmt.Fprintf(&b, "cwd        : %s\n", cwd)
	fmt.Fprintf(&b, "executable : %s\n", exe)
	fmt.Fprintf(&b, "PATH       : %s\n", os.Getenv("PATH"))
	b.WriteString("```")
	return b.String()
}

// inspectCat reads a file with the Go standard library (no `cat` binary needed).
// NUL bytes are rendered as newlines so /proc/*/environ files read cleanly.
func inspectCat(path string) string {
	path = filepath.Clean(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf(":x: cannot read `%s`: %s", path, err)
	}
	const maxBytes = 16 * 1024
	truncated := false
	if len(data) > maxBytes {
		data = data[:maxBytes]
		truncated = true
	}
	content := strings.ReplaceAll(string(data), "\x00", "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "**`%s`**\n```\n%s", path, content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```")
	if truncated {
		b.WriteString("\n_(truncated to 16 KiB)_")
	}
	return b.String()
}
