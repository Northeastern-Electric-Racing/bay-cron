# bay-cron-tui

A terminal dashboard for the bay-cron container. This page covers building it on your own machine and installing it on the bay server. For what the TUI does and its keybindings, see the [main README](../README.md#tui).

The bay server only gets the compiled binary, so it needs no Go installed. You build on your laptop, cross-compiling for the server (Linux, x86_64), and copy the result over.

## 1. Install Go

**macOS:**

```sh
brew install go
```

**Windows (PowerShell):**

```powershell
winget install GoLang.Go
```

Or use the installer from [go.dev/dl](https://go.dev/dl/). Open a new terminal afterward so `go` is on your `PATH`.

Check it works:

```sh
go version
```

## 2. Build

Run these from this `tui/` folder. `go mod tidy` downloads the dependencies the first time; after that you only need the build command.

**macOS (Terminal):**

```sh
go mod tidy
GOOS=linux GOARCH=amd64 go build -o bay-cron-tui .
```

**Windows (PowerShell):**

```powershell
go mod tidy
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -o bay-cron-tui .
Remove-Item Env:GOOS, Env:GOARCH
```

The last line clears the variables. Otherwise every later `go build` in that window keeps producing Linux binaries.

**Windows (Command Prompt):**

```bat
go mod tidy
set GOOS=linux
set GOARCH=amd64
go build -o bay-cron-tui .
set GOOS=
set GOARCH=
```

Either way, you get a file named `bay-cron-tui` with no `.exe` extension. That's expected, since it's a Linux program. It won't run on your laptop, only on the server.

## 3. Copy it to the bay

These commands use `scp` and `ssh`, which are built into macOS and Windows 10/11 (PowerShell or Command Prompt).

If you haven't already, add the server to your SSH config so you can call it `bay`. Put this in `~/.ssh/config` on macOS or `C:\Users\<you>\.ssh\config` on Windows:

```
Host bay
    HostName <bay-server-address>
    User <bay-server-user>
```

Ask a FinishLine lead for the address and user. Then copy the binary over and install it:

```sh
scp bay-cron-tui bay:~/
ssh -t bay "sudo install -m 755 ~/bay-cron-tui /usr/local/bin/bay-cron-tui && rm ~/bay-cron-tui"
```

`install` puts it on the server's `PATH` and makes it executable in one step. `-t` gives `sudo` a terminal so it can ask for the password.

## 4. Run it

On the server:

```sh
bay-cron-tui
```

Or straight from your laptop in one command:

```sh
ssh -t bay bay-cron-tui
```

The `-t` matters here too. Without it, the TUI has no terminal to draw in.

## Updating

Pull the latest code, then repeat steps 2 and 3. Installing over the old binary replaces it.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `cannot execute binary file: Exec format error` on the server | It was built for the wrong platform. Rebuild with `GOOS=linux GOARCH=amd64`. The server is x86_64, so even on an Apple Silicon Mac, use `amd64`, not `arm64`. |
| `permission denied while trying to connect to the Docker daemon` | Your server user isn't in the `docker` group. Ask a lead to add you with `sudo usermod -aG docker <user>`, then log out and back in. |
| `bay-cron directory not found` | The TUI looks in `~/Projects/bay-cron` by default. Pass `-dir /path/to/bay-cron`. |
| The screen looks garbled or the banner is missing | Make the terminal wider. The banner needs at least 70 columns. Or check that you connected with `ssh -t`. |