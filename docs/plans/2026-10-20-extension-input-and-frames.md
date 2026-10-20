# Extension browser: frames, keyboard, and hover

Status: bounded implementation completed and verified; cross-origin frame automation remains deferred at the prototype gate.
Baseline: `429cad9` on `fix/browser-snapshot-references`.

## Objective and decisions

Extend the existing native Extension backend without replacing the approach
that is working on the user's sites. Keep content scripts, a private runtime
bridge, dedicated browser profiles, and existing takeover controls. Do not add
CDP, the debugger permission, remote-debugging launch flags, or silent fallback
to Standard or Cloud.

The deliverable is useful, explicitly bounded support for frames, keyboard
operations, and DOM hover events. It is not a promise of native keyboard/mouse
equivalence or CAPTCHA-free browsing. Existing human takeover for verification
and uploads remains in force.

Use maintained upstream code where it actually fits this design. Browser Use
and Browser Harness use CDP; Playwright Extension and Nanobrowser use the
debugger API. Their complete drivers are not direct replacements under these
constraints. Review reusable DOM utilities and regression fixtures, recording
their source revision and license if adopted. Do not add another agent loop or
a Python/Node service just to obtain a small helper.

## Current behaviour

| Area | Current implementation | Required change |
| --- | --- | --- |
| Snapshots | Main document only; document/generation element references | Aggregate supported frames and retain exact routing information |
| Reads/actions | Worker sends to frame 0 | Resolve references to the owning document |
| `fill` | Replaces value and dispatches synthetic input/change | Preserve its current semantics |
| `type` | Appends text and dispatches synthetic input/change | Preserve append semantics; keyboard editing is separate |
| `press` | Public schema exists; Extension rejects it | Add a bounded key set and explicit targeting |
| `hover` | Public schema exists; Extension rejects it | Add DOM pointer/mouse event dispatch with honest result semantics |
| Screenshots | Active owned tab; five-second capture timeout | Exercise awake/display-sleep/resume behaviour; retain bounded errors |
| Ownership | Per-app/profile/run, takeover gate, epoch invalidation | Apply the same rules to every frame and new action |

## Phase 1 — Compatibility fixtures and implementation checkpoints

Build on `TestExtensionBrowserWithoutDevTools`, the worker's Node tests, and the
registered browser-tool tests. Keep tests local, deterministic, and isolated
from users' accounts and live browser profiles.

Add fixtures for:

- Plain inputs, textareas, selections, disabled/read-only fields, native forms,
  a controlled input, a basic editable element, and a custom event-driven menu.
- Same-origin, cross-origin, nested, duplicate-URL, hidden, removed, and
  navigating iframes. Two loopback origins provide cross-origin coverage.
- A JS hover menu and a CSS-only hover menu; their expected outcomes differ.
- Key handlers that cancel defaults, DOM replacements during events, and
  submissions that deliberately lose their response.

Prototype the two uncertain parts before committing to a public capability:

1. Mapping a permitted child document to its embedding frame and ancestor
   visibility/verification restrictions without a page-controlled command bus.
2. Keyboard defaults and hover effects achievable through content scripts.

Record each capability as supported, limited, or human-required, with a
reproducing fixture. An event being dispatched is not evidence that the
application accepted an action.

**Exit:** reproductions exist for the new capabilities and their known limits;
the current click/fill/read/takeover tests continue to pass.

## Phase 2 — Frame discovery, references, and routing

### Implemented approach and prototype outcome

The real-browser prototype confirms the current content script can access and
validate same-origin frame Documents, including nested and duplicate-URL frames.
A cross-origin frame's contentDocument is unavailable. Adding all-frame injection
and webNavigation enumeration alone would not establish the DOM embedding
relationship needed for ancestor visibility/verification checks.

For this delivery, retain main-frame injection and traverse same-origin Documents
from the existing isolated content script. This provides direct identity checks
and parent-chain validation without a new permission or a page-message transport.
The originally proposed browser-frame-ID broker is deferred with cross-origin
support; it is not needed for the supported same-origin scope.

Cross-origin automation is not claimed as completed. It requires a separate
verified association prototype under the same no-debugger constraints. The
current fixture proves such controls are omitted and marked human-required.

### References and snapshots

- Keep model-facing references opaque and accepting bare and `@` forms.
- Map each published reference to both its original Document object and its
  element. Verify ownerDocument is unchanged, including after DOM adoption.
- At dispatch, verify every containing frame is connected and still owns that
  exact Document; verify visibility, disabled/inert state, and verification
  restrictions throughout the containing chain for mutations.
- Restrict traversed documents to the same HTTP(S) origin as the main document.
  This also avoids treating document.domain relaxation or inherited/opaque
  about:blank/srcdoc documents as supported origin-policy cases.
- Group output by frame, include its origin, and provide a frame-root reference
  for text reads, scoped snapshots, and scrolling.
- Bound traversal to 16 frames, depth 4, and 300 total references. Keep existing
  output limits and explicitly mark omitted/unavailable frames. The synchronous
  same-origin traversal does not need concurrent extension RPCs.
- Clear references on each new snapshot, handback/run epoch change, or main
  document replacement. Child navigation/removal and adopted nodes fail their
  exact-document checks even without an intervening snapshot.
- Retain serialized mutations and the existing runtime takeover gate.

### Commands and scope

Click/fill/type/select/check/uncheck/hover resolve existing element references to
those exact Documents. A referenced read uses that element's document; an
unscoped read remains in the main document. Extension snapshots accept a current
frame reference through selector. Extension scroll accepts an optional frame or
element reference through ref. No new public tool or SDK lifecycle API is needed.

**Exit for this delivery:** verified same-origin routing, explicit cross-origin
limitation, exact-document invalidation, bounded discovery, and parent-chain
checks. Full cross-origin routing remains a separate gate.

## Phase 3 — Keyboard support

### Targeting and contract

- Add `press` to the native Extension allowlist only after its handler and
  tests exist.
- Require a current target reference for Extension key operations. This avoids
  guessing which frame owns browser focus. Explain this backend requirement
  in the tool description/error; the existing public `ref` field is sufficient.
- Carry that reference through Extension dispatch. Today the shared
  `browserActionArgs` press case drops it.
- Focus the verified target and confirm that focus succeeded before dispatch.
  Reject hidden, disabled, read-only editing targets, verification controls,
  and stale references as appropriate.
- Preserve Standard/Cloud press arguments and legacy callback behaviour.

### Initial supported operations

| Operation | Planned support and bounds |
| --- | --- |
| Backspace/Delete | Native text input/textarea selection and caret editing |
| Left/Right/Home/End, Shift selection | Tested text-control movement; reject unsupported modifiers/control types |
| Control+A / Meta+A | Select text in the referenced editable control |
| Enter in textarea | Insert a newline with the tested input-event contract |
| Enter/Space on a native button | Native DOM activation, respecting cancelled key events |
| Enter in a native form input | Only after implicit-submission fixtures validate submitter selection, validation, cancellation, and exactly-once behaviour |
| Escape and widget keys | Dispatch supported DOM key events; report dispatch separately from an observed application change |
| Tab/Shift+Tab | Add after focus-order fixtures pass; initially stay within a verified document and require takeover at unsupported boundaries |

Implementation must respect keydown cancellation, input-event cancellation
where applicable, selection state, and framework-controlled values. Use the
existing setter/input path where appropriate. Do not change `fill` or `type`
into per-character keypress operations.

Native form submission must use browser validation/submission mechanisms such
as `requestSubmit`, not `form.submit()` or a direct HTTP request. Never replay
submission when acknowledgement is lost. If a safe default action cannot be
determined, return an explicit unsupported result.

Clipboard access, OS/browser shortcuts, arbitrary modifier combinations, IME,
and full rich-text-editor compatibility are outside the initial supported set.
Do not manipulate clipboard contents to simulate typing.

**Exit:** supported editing operations produce the expected value/selection,
cancelled handlers are honoured, and submit/activation fixtures record at most
one operation. Unsupported keys fail before mutation.

## Phase 4 — DOM hover

- Add `hover` to the native allowlist after validating its handler.
- Resolve and revalidate the target using the same frame/reference rules.
- Scroll it into view; do not focus it merely to simulate hovering.
- Generate a tested pointer/mouse enter, over, and move sequence for the target
  and relevant ancestors. Handle leaving the prior target, including frame
  transitions, and clear hover bookkeeping on navigation/takeover.
- Refresh the snapshot after bounded rendering time so an opened JS menu can
  expose its controls.
- Report that DOM hover events were dispatched. Only report a menu as opened
  when its resulting state was observed.

Synthetic events do not move the system pointer or establish native CSS
`:hover`. CSS-only hover menus therefore remain a documented human-required
case under this architecture. Do not force CSS classes/styles or conceal a
switch to debugger/OS input. Do not claim automatic detection of every such
menu; the capability description must state the limit.

**Exit:** supported JS menus open and close correctly, their items are
actionable, and the CSS-only fixture is never reported as successfully hovered.

## Phase 5 — Outcome reporting and recovery

- Preserve a clear distinction between event dispatch, observed state change,
  unsupported operation, and uncertain completion.
- Add action-specific result detail to existing tool results where needed;
  avoid adding a generic state-tracking subsystem.
- Retry only safe reads after navigation. Do not retry keys, clicks, form
  submission, or other state-changing operations after uncertain delivery.
- A worker reconnect must not replay queued expired actions. In the delivered
  same-origin design, routing is owned by the content script and validated
  against live Documents; a worker reconnect alone loses no reference map.
  Run/handback epoch changes still clear it, and actual document loss requires
  a fresh snapshot.
- Bound worker configuration loading and bridge request/response bodies;
  reconnect with a one-to-five-second backoff after failure. Preserve the
  bounded normal worker cycle and Manifest V3 alarm fallback.
- Wait for an authenticated poll before queuing the initial native command.
  Separate the bounded startup wait from action execution, respect caller
  deadlines, and state explicitly when startup failed before sending anything.
- Extend takeover/handback and cancellation tests across child frames.
- Retain the five-second screenshot timeout. Verify subsequent commands work
  after failure and capture works again when the display is available.
- Separate display sleep from host sleep: the former may prevent capture; no
  local driver can execute while the host itself is suspended.

**Exit:** no false success for unsupported actions and no duplicate external
effect in navigation/disconnection tests.

## Phase 6 — Verification matrix

| Group | Required evidence |
| --- | --- |
| Existing functionality | Click, read, fill, append, empty/whitespace values, select, checkbox/radio handling, profile cookies |
| Frame routing | Same-origin, cross-origin where supported, nested, duplicate URL, hidden, removed, replaced, denied origin |
| Reference lifetime | New snapshot, child navigation, takeover, run replacement, worker restart |
| Keyboard | Text/selection results, cancellation, native form validation, custom handlers, unsupported shortcuts, no replay |
| Hover | JS menu entry/exit, menu item interaction, CSS-only limitation |
| Isolation | Wrong app/profile/run, foreign tab, spoofed page messages, expired command, verification/upload restrictions |
| Lifecycle | Relaunch, reconnect, display sleep/wake, host suspend/resume, screenshot timeout and recovery |
| Other backends | Standard/Cloud arguments and behaviour unchanged; legacy Extension callback contract preserved |

Use three layers: small JS/Go tests for routing and contracts; real Chrome
fixtures for DOM behaviour; packaged Rex smoke checks on macOS and Windows.
Use the available Windows VM but label its architecture/emulation accurately.
Do not claim native Windows x64 or Linux GUI verification without running it.
If server/Linux Extension support is in the release scope, include a real
virtual-display smoke run.

Run focused race tests, Go static checks, and the affected API/engine browser
tests. Run the full Go suite before release; compare failures against the same
platform's unchanged base and report any existing failures explicitly. Do not
retry tests repeatedly until green without understanding intermittent failures.

Live-site checks are a final compatibility check, not a substitute for
fixtures. Do not send invitations/messages or submit real forms just to test;
those require a separately authorized action.

## Code ownership and expected files

| Component | Responsibility |
| --- | --- |
| `internal/tools/browser_extension/manifest.json` | Frame injection and narrowly required permissions |
| `internal/tools/browser_extension/runtime-worker.js` | Discovery, document routing, reference map, deadlines and recovery |
| `internal/tools/browser_extension/content.js` | Local snapshot/target validation, supported keyboard defaults, DOM hover |
| `internal/tools/browser_tools.go` | Public tool descriptions and native Extension argument routing |
| `internal/tools/browser_extension_native.go` | Allowed commands and existing profile/run/takeover enforcement |
| Browser Go/JS tests | Contract, real-browser, isolation, and lifecycle coverage |
| `docs/browser-extension.md` | Actual supported behaviours and limits |
| Rex | Later runtime dependency update, packaging, and user-facing capability wording |
| Go SDK | No planned change; existing browser lifecycle contracts remain sufficient |

Keep new modules only where frame routing or input logic needs a coherent unit;
do not introduce a general browser framework. Reuse the existing bridge and
tool-result boundaries.

## Delivery order and release gates

1. Continue on `fix/browser-snapshot-references`, as explicitly requested by
   the user. Keep the already verified browser fixes in this branch.
2. Land fixtures and the frame-routing prototype; document unsupported cases.
3. Implement and verify frame routing, then keyboard, then DOM hover in
   separate reviewable commits.
4. Complete recovery tests and macOS/Windows verification. Update documentation
   to the capabilities demonstrated by those tests.
5. Push the feature branch for review. Do not silently enable unverified
   capabilities or change existing integrations' selected backend.
6. Merge/release agent-runtime through the established pipeline.
7. Update Rex to the released runtime, build and test packaged apps, then
   restart the local installation at the user's requested release step.

No profile migration, cookie export, database migration, or SDK release is
planned. Rollback should be a runtime-version rollback with existing browser
profiles intact; verify extension asset replacement and locked-profile handling.

## Upstream references

- [Chrome content scripts](https://developer.chrome.com/docs/extensions/develop/concepts/content-scripts):
  isolated worlds, frame injection, related-origin matching.
- [Chrome webNavigation](https://developer.chrome.com/docs/extensions/reference/api/webNavigation):
  frame enumeration and document identity across navigation.
- [Chrome tabs](https://developer.chrome.com/docs/extensions/reference/api/tabs):
  document-targeted messages.
- [DOM event trust](https://developer.mozilla.org/en-US/docs/Web/API/Event/isTrusted):
  synthetic events do not become trusted browser input.
- [Browser Harness](https://github.com/browser-use/browser-harness):
  CDP-based MIT-licensed driver/tooling.
- [Playwright Extension](https://github.com/microsoft/playwright/tree/main/packages/extension)
  and [manifest](https://github.com/microsoft/playwright/blob/main/packages/extension/manifest.json):
  existing-browser integration with debugger permission.
- [Nanobrowser](https://github.com/nanobrowser/nanobrowser):
  debugger-based interaction layer and Puppeteer frame tracking.


## Verification record — 2026-10-20

- macOS ARM64, Chrome for Testing 149.0.7827.115: real-browser public-tool
  fixtures and existing browser lifecycle checks run with race detection.
- Windows 11 ARM64 VM running the x64 test executable and x64 Chrome for Testing
  149.0.7827.115: final uninstrumented fixture, restart/cookie, launch-policy,
  startup-authentication, and native bridge tests passed (feature fixture
  5.90 seconds; lifecycle fixture 6.51 seconds). This is x64 emulation coverage,
  not a claim about native physical x64 hardware.
- Worker tests cover safe read retry, uncertain mutation non-replay, screenshot
  deadlines, private sender checks, startup reconnect/backoff, lost result
  acknowledgement, window readiness/deadlines, and skipping unnecessary saved
  page restoration during explicit navigation.
- The full macOS Go suite has the same nine pre-existing workspace/Python test
  failures reproduced on unchanged main; do not describe that suite as green.
- Windows verification initially exposed startup/restoration timeouts. The
  final delivery bounds configuration/bridge recovery, waits for authenticated
  readiness, targets an explicitly ready normal browser window, uses the
  command budget for restoration, and avoids loading the old page before an
  explicitly requested new URL. Temporary phase tracing is not shipped.
- Temporary Windows scheduled-task and credential-helper setup was removed.
  No real account, invitation, message, or production form was used for testing.

Still outside verified delivery: cross-origin/opaque frames, CSS-native hover,
full rich-editor/IME/OS keyboard equivalence, physical host suspend/resume,
Linux GUI execution, and native physical Windows x64. Rex dependency updates,
installer builds, and local Rex restart follow a released runtime version.
