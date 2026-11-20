<p align="center">
  <img src="assets/icon.svg" width="72" height="72" alt="">
</p>

# prized-go

**Unofficial Go SDK for [Prized](https://prized.dev) Sandboxes.**

Prized Sandboxes are Linux microVMs your code creates through an API: run commands, move files in and out, pause with memory, fork into live copies, expose ports, and delete. This module is an independent Go client for that API, with the same observable behavior as Prized's Python and TypeScript SDKs.

> [!NOTE]
> This is a community project. It is not affiliated with, endorsed by, or supported by Prized, Inc. "Prized" and the Prized logo belong to their owner.

**Status:** early (`v0.1.x`). Sandboxes are in beta at Prized. This client tracks **Python SDK 0.1.3** and **TypeScript SDK 0.1.4**. APIs may change before `v1.0.0`.

## Install

```bash
go get github.com/ijaidev/prized-go
```

Requires **Go 1.22 or newer**. There are no third-party runtime dependencies.

## Authentication

Create an API key at [prized.dev/dashboard/workspace/api-keys](https://prized.dev/dashboard/workspace/api-keys) with **Sandboxes: Write**, then export it:

```bash
export PRIZED_API_KEY=dcp_…
```

Or pass it explicitly:

```go
client, err := prized.NewClient(prized.WithAPIKey("dcp_…"))
```

`NewClient` fails before sending anything when no key is available.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	prized "github.com/ijaidev/prized-go"
)

func main() {
	client, err := prized.NewClient()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sb, err := client.Sandboxes().Create(ctx, prized.CreateSandboxOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := sb.Delete(context.Background(), &prized.DeleteOptions{Wait: true}); err != nil {
			log.Printf("delete %s: %v", sb.ID(), err)
		}
	}()

	result, err := sb.Exec(ctx, `python3 -c "print(6 * 7)"`, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(result.Stdout) // 42
}
```

## What it covers

| Area | Methods |
|---|---|
| Create | `Sandboxes.Create` with template, shape, env vars, labels, auto-pause, auto-delete, max runtime, idempotency key, `NoWait` |
| Discover | `Sandboxes.Connect`, `List`, `Templates`, `DeleteByLabels` |
| Commands | `Sandbox.Exec`, `ExecArgv`, `Start`, `StartArgv`; streaming, timeouts, output caps, background handles |
| Files | `Sandbox.Files().Write`, `Read`, `ReadBytes`, `Upload`, `Download`, `List`, `Remove`, `Exists` |
| Lifecycle | `Pause`, `Resume`, `Refresh`, `KeepAlive`, `Delete`, `Extend`, `SetTimeout` |
| Scale out | `Fork` a running or paused sandbox into live copies |
| Ports | `PreviewURL`, `PreviewHeaders`, `PreviewToken`, `SetPortPublic` |
| Failures | Typed errors, did-it-run classification, retries, `context` cancellation |

Not covered: Prized **Boxes** (the persistent dev machines), the `prized` CLI, and the MCP server.

## Creating a sandbox

```go
sb, err := client.Sandboxes().Create(ctx, prized.CreateSandboxOptions{
	Template:  "prized-agent-v1", // or prized-base-v1
	Resources: &prized.SandboxResources{VCPU: 2, MemoryMB: 4096, DiskGB: 20},
	EnvVars:   map[string]string{"CI": "1"},
	Labels:    map[string]string{"job": "nightly"},
	MaxRuntimeMin: 120,
	AutoPauseMin:  15,
	AutoDeleteMin: 60,
})
```

Omitted fields take the template defaults. `Create` returns once the sandbox is `running`; set `NoWait: true` to return at `starting`.

## Running commands

```go
result, err := sb.Exec(ctx, "npm test", &prized.ExecOptions{
	Cwd:     "app", // relative to /home/prized/work
	Env:     map[string]string{"CI": "1"},
	Timeout: 10 * time.Minute,
})
fmt.Println(result.Stdout, result.ExitCode) // non-zero exits are results
```

Use `ExecArgv` to let the SDK quote arguments:

```go
result, err := sb.ExecArgv(ctx, []string{"git", "commit", "-m", "it's done"}, nil)
```

Stream output as it arrives:

```go
var logs strings.Builder
result, err := sb.Exec(ctx, "npm test", &prized.ExecOptions{
	Stdout: &logs,
	Stderr: &logs,
})
```

Start a background command in its own session:

```go
handle, err := sb.Start(ctx, "python3 -m http.server 8000", nil)
if err != nil {
	log.Fatal(err)
}
defer handle.Cleanup(context.Background())

status, _ := handle.Status(ctx)
fmt.Println(status.Running)

killed, _ := handle.Kill(ctx, "SIGTERM")
fmt.Println(killed)

result, err := handle.Wait(ctx, nil) // nil timeout waits without a bound
```

A command that exceeds its timeout is killed and returns `*prized.CommandTimeoutError` with the partial result. Output is capped at 4 MiB per stream; pass `FullOutput: true` for up to 100 MiB.

## Files

```go
files := sb.Files()

mode := os.FileMode(0o755)
_, err := files.Write(ctx, "scripts/run.sh", []byte("#!/bin/sh\necho hi\n"), &prized.WriteOptions{Mode: &mode})

content, err := files.Read(ctx, "scripts/run.sh")
entries, err := files.List(ctx, "scripts")
exists, err := files.Exists(ctx, "scripts/run.sh")
err = files.Remove(ctx, "scripts/run.sh", nil)

_, err = files.Upload(ctx, "local.txt", "remote.txt", nil)
n, err := files.Download(ctx, "remote.txt", "local-copy.txt")
```

Paths are absolute or relative to `/home/prized/work`; transfers stay inside `/home/prized` and move at most 100 MiB per file. An interrupted write leaves the previous file untouched.

## Lifecycle and cleanup

```go
err = sb.Pause(ctx, nil)   // memory and disk kept; compute stops
err = sb.Resume(ctx, nil)  // every process continues where it stopped
err = sb.Refresh(ctx)      // re-read state and fields
err = sb.KeepAlive(ctx)    // reset the auto-pause clock

runTime, err := sb.Extend(ctx, prized.ExtendOptions{By: time.Hour})
runTime, err = sb.SetTimeout(ctx, prized.TimeoutOptions{In: 30 * time.Minute})

err = sb.Delete(ctx, &prized.DeleteOptions{Wait: true})
```

`Pause` and `Resume` wait for the requested state by default; set `NoWait: true` to return once the API accepts.

## Fork

```go
copies, err := sb.Fork(ctx, &prized.ForkOptions{Count: 2})
if err != nil {
	log.Fatal(err)
}
for _, copy := range copies {
	defer copy.Delete(context.Background(), nil)
}
```

Fork works on a running or paused parent, copies memory and processes, and returns only when every copy is running. On failure it deletes every copy it made, unless you supplied your own idempotency key.

## Ports

```go
url, err := sb.PreviewURL(3000)
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
for name, value := range sb.PreviewHeaders() {
	req.Header.Set(name, value)
}
resp, err := http.DefaultClient.Do(req)
```

Send `PreviewHeaders` only to the preview URL's own origin, and never follow a redirect with the key. The SDK's own transport refuses redirects; a custom `http.Client` must do the same.

Short-lived token for a browser or a client that cannot send headers:

```go
token, err := sb.PreviewToken(ctx, 3000, &prized.PreviewTokenOptions{ExpiresIn: 600})
fmt.Println(token.BrowserURL)
```

Make a port public to anyone with its URL:

```go
preview, err := sb.SetPortPublic(ctx, 3000, true, &prized.SetPortPublicOptions{ExpiresIn: 300})
```

Token links and public ports need a **paid workspace**. A preview request with the API key wakes a paused sandbox; a token or public URL does not.

## Discovery and cleanup

```go
running, err := client.Sandboxes().List(ctx, prized.ListSandboxesOptions{
	Labels: map[string]string{"job": "nightly"},
	States: []string{prized.StateRunning},
})

templates, err := client.Sandboxes().Templates(ctx)

ids, err := client.Sandboxes().DeleteByLabels(ctx, map[string]string{"job": "nightly"})
```

`List` returns non-deleted sandboxes newest first. Labels are ANDed; states are ORed. `DeleteByLabels` requires at least one label and never resends a delete whose answer was lost.

## Errors

Every API failure is a `*prized.Error`:

```go
var apiErr *prized.Error
if errors.As(err, &apiErr) {
	fmt.Println(apiErr.Code, apiErr.Status, apiErr.Reason)
	switch apiErr.Effect {
	case prized.EffectNotRun:
		// provably did not happen
	case prized.EffectMaybeRan:
		// the answer was lost; check before retrying
	case prized.EffectRan:
		// it happened
	}
}
```

Specialized failures:

- `*prized.CommandTimeoutError` — command killed at its timeout; `Result` has the partial output.
- `*prized.OutputReadError` — a `FullOutput` command ran, but its output could not be read back.
- `prized.ErrHandleClosed` — a handle was used after `Close`.

## Retries, timeouts, cancellation

- Retries `429` (honoring `Retry-After`), `502`, `503`, `504`, and dropped connections with backoff, up to `MaxRetries` (default 3).
- Reads retry other `5xx`; mutating calls are retried only when they provably never arrived.
- Balance, quota, and maintenance refusals are never retried.
- Every blocking call takes a `context.Context`; cancellation stops requests and polling.
- `WithRequestTimeout`, `WithMaxRetries`, `WithPollInterval`, and `WithPollOutage` tune the transport.

## Testing

The default suite uses an in-process fake API and never touches the network:

```bash
go test ./...
go test -race ./...
go vet ./...
```

Live tests create real sandboxes and spend credit. They are opt-in:

```bash
PRIZED_API_KEY=dcp_… go test -tags=live -v ./...
```

`TestLivePorts` requires a paid workspace for token and public-port links; on free credit it stops at `share_links_need_payment`.

## Scope

- Sandboxes only. No Boxes, CLI, MCP, or LangChain/OpenAI Agents adapters.
- Tracks the pinned Python and TypeScript sandbox SDK behavior; where their docs and this README differ, the prior art wins.
- Sandboxes run in one region (`us-west-2`), without GPUs, custom images, or resizing at the time of writing.

## Documentation

- [Prized Sandboxes docs](https://prized.dev/docs/sandboxes)
- [Sandbox API reference](https://prized.dev/docs/sandboxes-api)
- [Python SDK](https://pypi.org/project/prized/)
- [TypeScript SDK](https://www.npmjs.com/package/@prized/sdk)
- [Go package docs](https://pkg.go.dev/github.com/ijaidev/prized-go)

## License

Apache-2.0. See [LICENSE](LICENSE).
