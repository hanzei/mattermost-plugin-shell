# Shell

> ⚠️ **DANGER — DO NOT USE IN PRODUCTION** ⚠️
>
> This plugin puts an **interactive shell on the host machine** running the
> Mattermost server, reachable from chat — plus `/exec` and `/inspect` commands
> for one-off use. Although everything is restricted to system administrators, it
> effectively grants a remote shell on your server and has **no sandboxing, no
> audit trail, and no resource limits**. It is an experiment intended for labs and
> pre-prod environments only. Never deploy it to production or an internet-facing
> environment.

A Mattermost plugin that gives system admins an interactive shell on the host —
as a right-hand-sidebar terminal, plus `/exec` and `/inspect` slash commands.

## Usage

`/exec <command>` runs the command on the host via `sh -c` and returns the
combined stdout/stderr as an ephemeral message.

- Restricted to **system administrators**.
- Commands run as the OS user of the Mattermost server process.
- A command is killed if it runs longer than **30 seconds**.

```
/exec uname -a
/exec df -h | grep /dev/sda1
```

## Interactive terminal (RHS)

The plugin also provides a full interactive terminal in the right-hand sidebar.
Click the terminal button in the channel header to open it.

![Interactive host terminal in the right-hand sidebar, showing colored output](docs/terminal.png)

- Restricted to **system administrators** (enforced before the WebSocket upgrade).
- Spawns an interactive shell with a PTY on the host, as the Mattermost server
  process user.
- **Per-user shell selection:** a dropdown in the panel header lists the shells
  detected on the host (`bash`, `zsh`, `fish`, `sh`, `dash`, `ksh`, `tcsh`). The
  choice is saved per user in the KV store and reused on the next open. The
  detected set is also the allowlist — a requested shell that isn't detected is
  ignored and the default (bash, then sh) is used, so no client-supplied path is
  ever executed.

  ![Panel header with the per-user shell picker](docs/shell-picker.png)
- A generated rc file enables a colored prompt, `ls`/`grep` color aliases,
  dircolors, and bash programmable completion for `bash` (via `--rcfile`) and
  POSIX shells (via `$ENV`). Other shells (`zsh`, `fish`, `tcsh`) run
  interactively and use the host user's own dotfiles.
- Transport is a WebSocket through the plugin's HTTP path
  (`/plugins/com.hanzei.shell/api/v1/terminal`), using the framework's
  connection-hijack support. Output is streamed as binary frames; keystrokes and
  resize events are sent as JSON control messages.
- Live sessions are tracked and killed when the plugin is deactivated.

> This is an MVP: there is **no session isolation, recording, or resource
> limiting** yet. See the production checklist below before considering real use.

### Production hardening (not yet implemented)

The terminal is functionally complete but deliberately unhardened. Before any
non-local use it needs: process isolation (dedicated low-privilege user, or a
container/nsjail sandbox instead of the server process user), full session audit
logging / recording, idle and max-duration timeouts, and a concurrent-session
cap.

## Development

Requires Go and Node (see `.nvmrc`; run `nvm i` to match).

Build the plugin bundle:

```
make
```

This produces `dist/com.hanzei.shell-<version>.tar.gz`.

### Deploying to a local server

Enable [local mode](https://docs.mattermost.com/administration/mmctl-cli-tool.html#local-mode)
in your server config, then:

```
make deploy
```

```json
{
    "ServiceSettings": {
        "EnableLocalMode": true,
        "LocalModeSocketLocation": "/var/tmp/mattermost_local.socket"
    }
}
```

Alternatively, deploy by authenticating against the server API:

```bash
export MM_SERVICESETTINGS_SITEURL=http://localhost:8065
export MM_ADMIN_USERNAME=admin
export MM_ADMIN_PASSWORD=password
make deploy
```

To learn more about plugins, see the
[Mattermost plugin documentation](https://developers.mattermost.com/extend/plugins/).
