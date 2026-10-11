# Native Extension browser

Extension is built into agent-runtime. Apps select it through trusted agent
configuration; the existing browser tools keep their schemas:

```json
{"execution_config":{"browser":{"backend":"extension","profile":"user-123"}},"allowed_tools":["browser_open","browser_snapshot","browser_click","browser_close"]}
```

The runtime freezes the selected backend in durable run metadata. Incoming
metadata cannot override the policy. Standard and Cloud remain explicit choices;
errors never switch an Extension run to another driver. Other apps retain their
existing default backend.

## Installation and storage

Install compatible Chrome for Testing in the deployment image or with
`agent-browser install`. Runtime discovers that bundled browser without driving
it through agent-browser. Set `AGENT_RUNTIME_BROWSER_EXTENSION_EXECUTABLE` for
an explicit executable. No CDP connection or debugger permission is used.

Set `AGENT_RUNTIME_BROWSER_PROFILE_ROOT` to persistent storage. Profiles are
namespaced by app and profile identity, which defaults to the agent identity.
The profile key must be a single directory name. Only one agent owns a profile;
one active run can automate it. Existing profile locks fail closed.

`AGENT_RUNTIME_BROWSER_PROFILE_APP_ID` is a trusted migration setting: only that
app uses flat profile directories beneath the root. Rex sets it to `dost` and
uses existing bot slugs, preserving cookies without copying profiles. Other apps
remain namespaced. Do not use this mapping to share profiles across tenants.

Extension assets, credentials and held state are private runtime files. The
loopback bridge has per-profile random credentials and validates extension
origins, pending command IDs, deadlines and epochs. The runtime worker uses a distinct script URL from the legacy Rex worker,
avoiding Chrome reusing its cached script during migration. It reads fresh
credentials after restart. Credentials are never returned by the control API.

## Human controls

Use the existing runtime service authentication:

- `GET /v1/browser/{agent_id}?app_id={app}` returns backend, active bridge,
  running process, held state and automation run ID.
- `POST` to the same URL accepts `{"action":"open"}`, `takeover`, `handback`
  or `close`.

Controls require Extension policy and browser access for the agent. Open and
Takeover wait for the current operation and persist the held gate. Handback
invalidates references. Close requires a stopped run or prior takeover. These
are host controls, not model tools. Apps must authorize their users before
calling them. Held state survives runtime restart.

Pausing or ending a run releases its automation lease without closing the page.
Paused sessions remain resumable, and a held browser still needs explicit handback. The configured browser
session timeout eventually closes unheld idle processes. Shutdown closes owned
processes; reopening restores the saved URL and cookies. A crash leaving Chrome
running requires closing that window before reopening its locked profile.

## Capabilities and server deployment

The initial driver supports main-frame DOM snapshots, navigation, text, form
input, scrolling, bounded waits and visible-tab screenshots. Screenshots require
the owned tab to be active. Unsupported commands return explicit errors. Safe
reads and snapshots may retry during the five-second navigation bound; uncertain
clicks and submissions are never replayed. Humans handle sign-in, visible
verification, uploads and unsupported embedded frames.

On Linux servers provide a visible Chromium session on a virtual display and
persistent profile storage. Remote takeover needs an app-owned authenticated
noVNC or WebRTC display/input channel, isolated per user and routed to the same
worker. Runtime supplies the automation gate; it does not supply remote desktop
hosting or inherit desktop cookies. This deployment still needs Linux smoke
verification before rollout.

## Checks and compatibility

```sh
go test -race ./internal/tools ./internal/api ./internal/engine -run 'Test(Browser|Extension|Native|StartRunFreezesTrustedBrowserPolicy)'
AGENT_RUNTIME_EXTENSION_E2E=1 go test ./internal/tools -run TestExtensionBrowserWithoutDevTools -count=1 -v
```

The real-browser test uses a temporary profile and localhost fixture, covering
navigation, synthetic input, screenshots, references, takeover and full runtime
restart with cookies retained. The old v0.13.11 host callback remains temporarily
for existing consumers; native Extension needs neither host URL nor host token.
Publish this runtime release before consumers remove their callback implementation.
