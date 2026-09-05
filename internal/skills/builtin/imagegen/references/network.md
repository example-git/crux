# Authentication and permission behavior

Read this file if the native `imagegen` tool fails or asks for approval.

## Why approval appears

The native tool makes an outbound request to an image endpoint and writes one or more workspace files. Crux asks for one typed image-operation approval after validating and canonicalizing the request. Edit inputs outside the workspace also require external-read approval.

This is not a Bash-command approval. Do not relaunch Crux or route generation through Bash to avoid or reproduce it.

## Backend and credential resolution

When `backend` is omitted or set to `auto`, the native image client tries credentials in this order:

1. The active account from the configured Codex provider, including refresh through the shared provider account path. Requests go to the ChatGPT-backend Codex image endpoint.
2. The configured OpenAI API provider account. Requests use that account's resolved API key and endpoint.

When `backend` is `flow`, Crux uses Google Flow's unofficial direct non-Agent still-image operation and does not try Gemini Chat, Agent mode, video, Codex, or OpenAI. It scans supported local Chromium-family and Firefox profiles on macOS, Linux, and Windows, reads Google cookies from the browser database, and uses the operating system's browser-cookie decryption mechanism when required. A usable imported authentication session is cached only in memory for the current Crux process, and only one Google Flow job executes process-wide at a time. Cookie values and challenge tokens are not written to Crux configuration, provider accounts, task records, output files, or the upload registry. The registry retains only bounded project-scoped media IDs and safe file metadata keyed by content hash, and Flow must confirm a retained media ID before reuse.

## Missing credentials

For `auto`, tell the user to sign in with Codex through Crux or configure an OpenAI API account, then retry. Never ask the user to paste an API key in chat.

For `flow`, tell the user to sign in to Google Flow in a supported local browser, then retry. Never ask the user to paste browser cookies in chat. Chromium v11 cookie decryption on Linux requires an available, unlocked Secret Service session.

## Safety

The tool bounds request inputs and responses, validates output paths before queueing, refuses existing outputs unless replacement was explicitly approved, and supports cancellation through the shared task service. Google Flow image downloads require trusted Google HTTPS hosts and reject an untrusted redirect before following it. Treat any proposed broader network or filesystem access as a separate operation requiring its own justification and approval.

Google Flow is an unofficial, undocumented integration and may stop working when the web application changes. An explicit `flow` request does not silently switch routes or backends. Pro alone may fall back once to an available Nano Banana 2 model when Pro is absent from discovery or every Pro generation RPC fails before any output; partial output and authentication, challenge, upload, response-validation, download, or cancellation failures remain terminal.
