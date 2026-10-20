# Native Extension browser

Extension is built into agent-runtime. Apps select it through trusted agent
configuration; the existing browser tools keep their schemas:

```json
{"execution_config":{"browser":{"backend":"extension","profile":"user-123"}},"allowed_tools":["browser_open","browser_snapshot","browser_read","browser_act","browser_close"]}
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
Initial native commands wait up to 35 seconds for an authenticated extension
poll before they are queued. Command execution then has its own 35-second bound;
both waits respect caller cancellation/deadlines. A startup timeout explicitly
states that no command was sent. Worker configuration loading is bounded to five
seconds, bridge requests to 25 seconds, and failed connections retry with a
one-to-five-second backoff. Connection retries never replay delivered commands.

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

The driver supports main-document and bounded same-origin HTTP(S) frame
snapshots, navigation, text, form input, scrolling, bounded DOM keyboard input,
DOM hover events, bounded waits, and visible-tab screenshots. Screenshots require
the owned tab to be active and a usable display; stalled capture returns an explicit error within five
seconds instead of holding the command queue until the bridge times out.
Unsupported commands return explicit errors. Safe
reads and snapshots may retry during the five-second navigation bound; uncertain
clicks and submissions are never replayed. Humans handle sign-in, visible
verification, uploads and unsupported embedded frames. Cross-origin, opaque,
and inaccessible frames are explicitly marked unavailable. This version keeps
all frame traversal in the existing content script: it adds no debugger,
webNavigation permission, or page message channel.

Element actions and text reads accept the latest snapshot reference with or
without `@`; references become invalid after navigation, a new snapshot, or
handback. Reads may inspect disabled controls, but actions still reject them.
Form values preserve whitespace. An explicit empty `value` clears a field with
`fill`, or selects an option whose value is empty; an omitted value is rejected.
Checkbox changes are checked after the click. A selected radio cannot be
unchecked directly: select another option in its group instead.

Frame snapshots include an origin and a frame-root reference. Use that reference
with `browser_read` to read the frame, with `browser_snapshot.selector` to scope
the next snapshot, or with `browser_act` scroll to scroll it. Element references
target the exact same-origin document in which they were observed. A hidden,
removed, replaced, or verification frame cannot receive actions from an old
reference. Traversal is limited to 16 frames, depth 4, and 300 total references.

Extension `press` requires a current control `ref` as well as a key in `value`.
Supported text controls can edit/delete text, select all with Control+A/Meta+A,
move the caret with Left/Right/Home/End and Shift selection, and insert a newline
in a textarea. Tab/Shift+Tab follow focus order inside one document; moving
across a frame or document boundary requires a fresh target reference. Enter
and Space can activate native buttons; Enter can request a validated native
form submission with an unambiguous default submitter. Cancelled key/input
events are respected. Escape/Up/Down dispatch DOM key events; native default
behaviour is not assumed. Clipboard/browser/OS shortcuts and rich-editor defaults
require takeover. `fill` still replaces text and `type` still appends it.

Extension hover dispatches DOM pointer/mouse events without focusing the target
or moving the system pointer. It supports event-driven menus; CSS-only `:hover`
and controls requiring trusted input still need takeover. The tool returns an
`action_result` with its untrusted-content boundary and an `effect`, such as
`value_changed`, `selection_changed`, `focus_changed`, `cancelled`, or
`events_dispatched`. `activation_dispatched` does not establish a form was
accepted or an external effect was completed. Inspect the refreshed snapshot
and page state before reporting completion. A failed post-action snapshot
explicitly warns against blindly repeating the action.

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
navigation, synthetic input, form clearing/appending, selection, checkbox state,
read references, screenshots, takeover and full runtime
restart with cookies retained. Keep the display awake for screenshot checks
(on macOS, run the test under `caffeinate -du`). The old v0.13.11 host callback remains temporarily
for existing consumers; native Extension needs neither host URL nor host token.
Publish this runtime release before consumers remove their callback implementation.

`TestExtensionInputAndFrames` exercises the registered public tools with real
Chrome, including duplicate/nested frames, visibility and document replacement,
keyboard selection and cancellation, controlled values, form validation,
same-document focus order, and event-driven hover menus. Cross-origin frames
remain a tested explicit limitation; these features do not imply equivalent
native input or a guarantee about any site's CAPTCHA behaviour.
