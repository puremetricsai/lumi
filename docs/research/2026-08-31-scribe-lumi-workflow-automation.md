# Scribe research and a Lumi workflow-automation path

**Date:** 2026-08-31  
**Scope:** Read-only product and architecture research. No product implementation changes were made.

## Executive summary

Lumi can already approximate part of Scribe's value as a private, retrospective workflow analyst, but it
cannot produce Scribe-quality step-by-step guides reliably from its current evidence.

The products observe different things:

- **Scribe records actions:** clicks, typed entries, page or screen changes, exact click positions, and
  screenshots during a bounded workflow.
- **Lumi records states:** periodic display screenshots, full-display OCR, focused app/window attribution,
  and system/microphone transcripts during ambient recording.

That distinction matters more than adding AI. Scribe says its core automatic guide creation does not use
AI. Its accuracy comes from structured interaction capture; optional AI generates titles, descriptions,
larger process documents, and improvement recommendations.

The best way to use Lumi now is a **voice-assisted workflow review**:

1. Say explicit start and end markers while Lumi records.
2. Narrate meaningful actions and decisions without speaking secrets.
3. Use an MCP-connected agent to bound the session from the transcript, inspect screen events in that
   range, and draft an evidence-backed SOP.
4. Compare several narrated executions to identify repetition, app switching, manual transfers, exceptions,
   and automation candidates.
5. Hand the resulting automation brief to a separate actuator such as an existing API/CLI, macOS Shortcuts,
   AppleScript, or browser automation.

Lumi should remain the observation and evidence layer. Actual execution should stay behind a separate,
explicitly authorized boundary.

## What Scribe is actually automating

Scribe primarily automates **process documentation**, not the underlying business task.

Its documented capture flow is:

1. Install the Chrome/Edge extension or desktop application.
2. Start capture and perform the process.
3. Stop capture.
4. Receive an automatically generated guide containing screenshots, text, and cursor clicks.
5. Edit, redact, reorder, export, embed, or share the guide.

Scribe's public documentation establishes the input signals:

- The extension detects clicks, typed entries, and page changes.
- AutoCapture records clicks, keystrokes, and screen changes on whitelisted domains.
- A click target is placed at the exact location of each captured click.
- Optional voice narration is transcribed and applied to corresponding steps.
- The browser extension uses HTML to derive high-quality labels.
- The desktop application uses operating-system Accessibility metadata, whose quality varies by application.

Scribe does not publish enough implementation detail to reconstruct its complete internal pipeline. A
reasonable behavioral model, based on those documented inputs and outputs, is:

```text
bounded capture
  -> structured interaction events
  -> action-triggered screenshots
  -> element labels from HTML or Accessibility
  -> ordered guide steps and click targets
  -> optional narration and AI enrichment
  -> editable/shareable guide
```

The distinction between browser and desktop capture is important. Scribe explicitly says browser labels are
more accurate because websites share HTML, whereas desktop vendors expose inconsistent Accessibility
metadata. A Scribe-like product that cares primarily about web workflows eventually needs browser-level
signals; adding more OCR to a desktop recorder does not produce equivalent control identity.

## Scribe's AI and workflow-improvement layers

Scribe separates several capabilities that are easy to conflate.

### Core capture

Scribe's security documentation states that automatically creating step-by-step guides is core functionality
and does not use AI. Structured capture is the foundation.

### Scribe AI and Pages

Optional AI features can:

- generate titles and descriptions;
- create larger SOPs, onboarding guides, and help centers;
- combine multiple Scribes with additional who/what/when/where/why context;
- improve grammar, tone, spelling, length, and summaries.

Scribe's documentation says the principal advantage over a generic chat model is access to the customer's
selected Scribes as organization-specific process context.

### Improve workflows with AI

This feature analyzes the steps of an existing Scribe against common patterns and best practices. It may
suggest:

- eliminating redundant or duplicate actions;
- automating repetitive steps already supported by existing systems;
- standardizing process variants;
- addressing bottlenecks;
- reducing manual work or error risk.

It does **not** rewrite or execute the underlying workflow. The user chooses which recommendations to act on.

### Scribe Optimize

Scribe markets Optimize as workflow/task/process mining across approved applications. Its published outputs
include:

- automatically discovered workflows and process maps;
- frequency and time-loss visibility;
- bottlenecks and inconsistencies;
- process-standardization opportunities;
- tool adoption and configuration recommendations;
- non-AI and agentic automation candidates;
- ROI projections and business cases.

This is a broader enterprise analysis product, not merely the guide generator.

### Scribe MCP

Scribe's hosted MCP exposes three useful layers to compatible agents:

- documents and guides, including screenshots;
- discovered workflows and execution history;
- optimization insights and recommendations.

This is materially different from Lumi's MCP, which exposes raw captured history rather than first-class
guide, workflow, and insight objects.

## Privacy and deployment differences

Scribe's help center says Scribes cannot be stored locally; they must be hosted in Scribe's AWS
infrastructure to support the workspace and link sharing. Scribe documents encryption, redaction,
administrative controls, and AI-provider restrictions. Its security page says AI providers do not receive
Scribe images, cannot train on customer data, and delete processed data within 30 days.

Lumi keeps capture, OCR, transcription, media, and indexing on the Mac. Lumi itself performs no inference.
Its MCP returns text, metadata, and local media paths, never screenshot or audio bytes.

That privacy boundary needs precise wording: media stays local through Lumi's MCP, but OCR and transcript
text returned to a hosted agent may leave the machine through the user's chosen MCP client/model. A fully
local model can keep that inference local; Lumi does not decide which model the user connects.

## Lumi's current evidence model

The relevant Lumi path is:

```text
ScreenCaptureKit displays -> JPEG -> frame comparison -> Vision OCR ----+
Accessibility focused app/window ---------------------------------------+-> events + FTS5
ScreenCaptureKit system/microphone -> WAV -> SpeechAnalyzer ------------+
                                                                       -> read-only MCP
```

The current capabilities are:

- all-display capture at a configurable interval, two seconds by default;
- SHA-256 exact-frame and sampled color-histogram near-duplicate detection;
- changed-but-similar frame checkpoints at most ten seconds apart;
- byte-identical presence checkpoints at most five minutes apart;
- full-display Apple Vision OCR;
- focused app and window attribution from Accessibility/window metadata;
- separate system and microphone audio capture;
- on-device Apple SpeechAnalyzer transcription;
- ordered, origin-attributed transcript assembly;
- SQLite FTS5 search;
- four read-only MCP tools: `search_events`, `get_event`, `list_apps`, and `get_transcript`.

Important interpretation rules:

- Screen OCR covers the **entire display**; `app` and `window` describe the focused application. Visible text
  from another window can therefore be stamped with the focused app.
- One focused-window snapshot is stamped onto every display at a capture tick. On multiple displays, an app
  filter can return a frame from a display where that app was not visible.
- Frame retention is state-change based, not action-triggered. A short-lived menu, dialog, toast, or page
  state can appear and disappear between captures.
- A recent-input boolean only raises frame-comparison sensitivity. Lumi does not persist or expose the
  input event, key, click, coordinate, or control.
- Deduplication makes event counts a poor proxy for time spent. Explicit start/end markers provide safer
  duration bounds.
- Audio transcripts can provide intent and explanation, but they are not currently attached to particular
  screen actions or guide steps.

## Capability comparison

| Capability | Scribe | Lumi today | Consequence |
| --- | --- | --- | --- |
| Capture boundary | Explicit or AutoCapture-detected workflow | Ambient recording | Lumi needs time or spoken boundaries |
| Primary evidence | Structured actions | Periodic screen/audio states | Lumi reconstructs rather than observes steps |
| Screenshot trigger | User actions | Timer plus visual deduplication | Transient states can be missed |
| Click location | Exact captured coordinate | Not recorded | No trustworthy click target |
| UI control identity | HTML or Accessibility element | OCR text only | Cannot prove which visible label was activated |
| Typed values | Captured subject to platform/privacy behavior | Only visible later through OCR | Fast, masked, or overwritten input is unavailable |
| Browser URL/DOM | Extension access | None | No selectors, URLs, or page structure |
| Voice context | Applied to corresponding steps | Chronological transcript | Agent must align it heuristically |
| Generated guide | First-class editable object | Agent-generated text draft | No local guide editor or artifact model |
| Images through MCP | Hosted screenshot URLs | Local path only | Visual guide assembly is manual by default |
| Workflow mining | Optimize | Bounded agent inference | Candidate discovery, not reliable process mining |
| Execution | Recommendations, integrations/context | None; MCP is read-only | Separate actuator required |
| Storage | Scribe cloud | Local media and SQLite | Lumi has the stronger local-first foundation |

## What works with Lumi now

### 1. Voice-assisted SOP capture

The user can supply the semantics Lumi lacks by speaking deliberate markers:

```text
Start workflow: weekly vendor invoice reconciliation.
Step: export last week's invoices from the finance portal.
Decision: if the difference exceeds fifty dollars, flag it for review.
Step: copy the approved total into the accounting spreadsheet.
End workflow: weekly vendor invoice reconciliation. Three exceptions found.
```

This is analogous to Scribe's optional narration, but the agent performs the alignment after capture rather
than Lumi attaching speech to action events in real time.

Narration should describe intent, actions, and decisions without speaking passwords, tokens, personal data,
or confidential values unnecessarily.

### 2. Evidence-backed SOP reconstruction

An MCP-connected agent can:

1. call `get_transcript` first to find spoken start/end markers;
2. call `search_events` for screen events in that exact range;
3. use `collapse_similar` to reduce repeated screens;
4. fetch a specific full event with `get_event` where needed;
5. return an ordered Markdown draft with timestamps, app/window, event IDs, and confidence;
6. name local screenshot paths for manual review or insertion.

The agent must not claim a specific click, keystroke, URL, field, or control unless evidence outside Lumi
supplies it.

A useful confidence convention is:

- **High:** narration names the action and the following screen state corroborates it.
- **Medium:** app/window transition and OCR state strongly imply the action, without narration.
- **Low:** a plausible action is inferred only from before/after states.

### 3. Repeated-run comparison

Several deliberately marked executions can reveal:

- steps that recur in every run;
- common application and window transitions;
- manual transfers between applications;
- repeated downloads, exports, searches, copying, and reformatting;
- exception paths and human decisions;
- inconsistent variants of the same outcome;
- approximate duration measured between explicit markers;
- opportunities to use existing product features, APIs, or scripts.

This remains a bounded analysis. The current MCP has no sequence aggregation or first-class workflow model,
and ambient event pages can be large. Claims about occurrence count or average duration should come from
explicitly identified runs, not raw event counts.

### 4. Automation briefs

Lumi can provide evidence for a brief that records:

- workflow goal and trigger;
- stable deterministic steps;
- data transferred between systems;
- decisions requiring a person;
- exception and rollback behavior;
- missing URLs, DOM selectors, API endpoints, or control identifiers;
- recommended actuation mechanism;
- baseline duration and error evidence.

This is the clean handoff from observation to implementation.

### 5. Before/after validation

Lumi is also useful after an automation exists. Explicit markers can compare:

- completion duration;
- number of manual application transitions;
- exception frequency;
- repeated manual cleanup;
- observable errors or retries;
- whether the intended final state appeared.

Lumi therefore has value as an automation observability layer even if it never executes anything.

## Suggested prompts

### Reconstruct one workflow

```text
Use Lumi to find my most recent "weekly invoice reconciliation" session.

Start with the transcript to identify its spoken start and end. Then inspect
screen events within that exact range.

Produce a Markdown SOP with:
- ordered steps;
- timestamp, app/window, and Lumi event IDs supporting each step;
- confidence of high, medium, or low;
- unclear or apparently missing transitions;
- local screenshot paths worth reviewing manually.

Do not invent clicks, keystrokes, field names, URLs, or UI controls that the
evidence does not establish.
```

### Compare repeated executions

```text
Compare my last five narrated runs of "weekly invoice reconciliation."

Identify:
- steps common to every run;
- variations and exception paths;
- repeated copying, downloading, reformatting, or app switching;
- delays measured only between explicit start/end or step markers;
- steps likely supported by an existing API, CLI, built-in application
automation, or browser automation.

Return automation candidates ranked by frequency, time saved, stability, and
risk. Treat OCR-derived actions as hypotheses, not confirmed clicks.
```

### Produce an automation brief

```text
Turn the strongest candidate into an automation brief, not implementation.

Include:
- trigger and expected result;
- deterministic steps;
- decision points requiring a human;
- data being transferred between applications;
- missing URLs, selectors, API endpoints, or permissions Lumi cannot observe;
- failure and rollback requirements;
- simplest suitable actuator: existing API/CLI, macOS Shortcuts, AppleScript,
  browser automation, or an AI agent.
```

## Choosing an automation mechanism

Use the least fragile mechanism that reaches the task:

1. Existing application automation or configuration.
2. API or CLI.
3. macOS Shortcuts.
4. Browser automation when no API exists.
5. AppleScript or Accessibility UI scripting for desktop-only interfaces.
6. An AI agent only where judgment or unstructured interpretation is genuinely required.

Stable data interfaces are preferable to replaying screen coordinates. A guide may say where a person
clicks, but a durable automation should use an API, semantic browser selector, or native application command
where available.

## What Lumi cannot infer reliably today

The following outputs would overstate Lumi's evidence:

- “The user clicked **Approve**.”
- “The user entered `1234` into the amount field.”
- “The workflow visited this exact URL.”
- “This OCR label is the control that caused the next state.”
- “These two similar screen sequences are definitely the same workflow.”
- “Twenty captured events means twenty seconds of work.”
- “The focused app produced all text visible in this frame.”

A safer form is:

> The display changed from state A to state B while application X was focused; nearby narration described
> approving the item. Lumi did not record the control activation itself.

## Minimal product path toward Scribe-like behavior

If Scribe-like capture becomes a Lumi goal, the shortest useful path is not an inference provider. It is
better evidence.

### P0: Guided workflow capture

Add an explicit, bounded mode with Start, Step, and End markers. Voice and a global hotkey can provide a
small, understandable surface. The result should be a session boundary over existing events, not a second
capture pipeline.

### P1: Structured, opt-in action evidence

During a guided session only, capture the minimum evidence needed to identify a step:

- click timestamp and coordinate;
- active application/window;
- Accessibility element role and label where available;
- before/after event linkage.

Raw keystroke logging should not be the default. Field identity and a marker that input occurred are safer
than storing the value; credentials and secure fields must remain excluded. Detailed interaction capture
should be session-scoped and allowlisted rather than ambient.

This would require a deliberate product decision because Lumi currently does not require an event tap and
its Input Monitoring permission is informational/optional.

### P2: Browser-first evidence where fidelity matters

For web workflows, a domain-allowlisted browser extension can provide:

- URL and navigation boundaries;
- DOM element role, label, and stable selector candidates;
- clicks, submissions, and page transitions;
- safer detection of password and sensitive fields;
- higher-quality step descriptions than desktop Accessibility alone.

This conflicts with Lumi's current deliberately narrow “no plugins” posture, so it is a strategic choice,
not an incidental feature. If no extension is acceptable, expectations must remain below Scribe's browser
fidelity.

### P3: Local guide artifact

Introduce a first-class local object only after the action evidence exists:

```text
Guide
  -> bounded source session
  -> ordered steps
  -> linked event and screenshot
  -> instruction, confidence, and optional click target
```

A minimal surface would support review, correction, redaction, and Markdown/HTML export. Collaboration,
branding, analytics, hosted sharing, and a full knowledge-base product are unnecessary for personal
workflow discovery.

### P4: Repeated-run analysis

Mine confirmed guided sessions rather than ambient OCR frames. Useful outputs are:

- common and variant step sequences;
- time between confirmed steps;
- repeated manual transfers;
- loops, retries, and exception branches;
- process standardization candidates;
- automation opportunities with supporting runs.

Start with deterministic local heuristics and agent analysis. A clustering or ML subsystem is justified only
when real session volume and failures show simpler sequence comparison is insufficient.

### P5: External actuation boundary

Export a structured automation brief or skeleton, but keep execution separate. Lumi's memory contains broad,
sensitive context; coupling it directly to privileged write actions would combine observation and authority
in one process. An external actuator provides a clearer consent, credential, and failure boundary.

## Product recommendation

There are two distinct product directions:

1. **Local Scribe:** exact guided documentation, screenshot editing, and local export.
2. **Personal workflow intelligence:** discover repetition, produce automation briefs, and validate results.

If the primary goal is helping one user automate their own work, the second direction is smaller and better
aligned with Lumi. It does not need Scribe's hosted workspace, collaboration, analytics, branding, or Pages.
It needs explicit workflow boundaries, trustworthy structured actions, repeated-run comparison, and a clean
handoff to existing automation tools.

The immediate no-code workflow—spoken markers plus Lumi MCP analysis—is sufficient to test whether users get
value from that direction before building capture or guide infrastructure.

## Repository evidence reviewed

- `CLAUDE.md`
- `README.md`
- `docs/architecture.md`
- `internal/capture/CLAUDE.md`
- `internal/capture/recorder.go`
- `internal/capture/compare.go`
- `internal/capture/screen.go`
- `internal/macosnative/native.m`
- `internal/store/CLAUDE.md`
- `internal/store/store.go`
- `internal/mcp/CLAUDE.md`
- `internal/mcp/server.go`
- `internal/mcp/tools.go`
- `internal/cli/CLAUDE.md`
- `macos/CLAUDE.md`

## External sources

Accessed 2026-08-31.

- [New User Guide — Scribe 101](https://support.scribehow.com/hc/en-us/articles/8951146003741-New-User-Guide-Scribe-101)
- [How to capture a Scribe using the extension](https://support.scribehow.com/hc/en-us/articles/13546388647453-How-to-capture-a-Scribe-using-the-extension)
- [Why the browser extension writes more accurate instructions](https://support.scribehow.com/hc/en-us/articles/11865867639581-Why-does-the-browser-extension-write-more-accurate-text-instructions-than-the-desktop-application)
- [How to use voice transcription](https://support.scribehow.com/hc/en-us/articles/23111152407069-How-to-use-voice-transcription-while-creating-a-Scribe)
- [Screenshots: Click Target](https://support.scribehow.com/hc/en-us/articles/7006817910429-Screenshots-Click-Target)
- [Using AutoCapture](https://support.scribehow.com/hc/en-us/articles/30708953411229-Using-discover-workflows)
- [Improve your workflows with AI](https://support.scribehow.com/hc/en-us/articles/29738561345053-Improve-your-workflows-with-AI)
- [Scribe AI: Overview and FAQs](https://support.scribehow.com/hc/en-us/articles/10842126103965-Scribe-AI-Overview-and-FAQs)
- [Scribe Optimize](https://scribe.com/optimize)
- [Scribe MCP Server](https://support.scribehow.com/hc/en-us/articles/35221245251485-Scribe-MCP-Server)
- [Scribe security and privacy](https://scribe.com/security)
- [Can Scribes be stored locally?](https://support.scribehow.com/hc/en-us/articles/9887805745821-Can-Scribes-be-stored-locally)
- [Exporting a Scribe to Markdown](https://support.scribehow.com/hc/en-us/articles/9254133020189-Exporting-a-Scribe-to-Markdown)
