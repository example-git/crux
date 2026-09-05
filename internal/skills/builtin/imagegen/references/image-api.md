# Image API context

These parameters describe the endpoints behind Crux's native `imagegen` tool. Call the tool rather than invoking an endpoint, SDK, script, or hidden CLI directly.

## Authentication paths

- With `backend: auto`, a usable configured Codex account uses the ChatGPT-backend Codex image endpoints and defaults to `gpt-image-2`.
- Otherwise, `backend: auto` uses the configured OpenAI API account and defaults to `gpt-image-1`.
- With `backend: flow`, Crux imports a Google session from a supported local browser and uses Google Flow's direct non-Agent still-image operation. `nano-banana-pro` is the default. If Pro is unavailable or every Pro generation RPC fails before any image is produced, Crux falls back once to `nano-banana-2` when Flow exposes it. The imported session remains only in memory for the current Crux process.
- Codex edit inputs are sent as bounded inline data URLs. OpenAI API edit inputs use bounded multipart uploads. Google Flow edit inputs use its direct authenticated image-upload operation; project-scoped media IDs are securely retained by content hash and revalidated before reuse.
- An explicit `flow` request never falls back to Gemini Chat, Agent mode, video, Codex, or OpenAI.

## Parameters

- `backend`: `auto` or `flow`; omitted means `auto`
- `prompt`: text prompt
- `model`: optional image model override; Google Flow defaults to `nano-banana-pro` and supports the bounded `nano-banana-2` fallback described above
- `n`: output count from 1 through 10
- `size`: `auto` or `WIDTHxHEIGHT`
- `quality`: `low`, `medium`, `high`, or `auto`
- `background`: `transparent`, `opaque`, or `auto`

`generate` is text-only. Pass every edit target or style, composition, mood, or subject reference through edit mode's ordered `images` list, then identify each image's role by index in the prompt. The native tool does not expose a mask parameter.

For Google Flow, omit `quality` and `background`, or use `auto`; explicit values fail because the direct operation does not execute those controls. `size: auto` omits the aspect control. Explicit dimensions must reduce to 1:1, 4:3, 3:4, 16:9, or 9:16 and select that aspect ratio without guaranteeing exact output pixels. Partial Pro success and challenge, upload, response-validation, download, or cancellation failures do not trigger fallback. Nano Banana 2 is never retried through another model. Only one Google Flow job executes process-wide at a time.

## Popular `gpt-image-2` sizes

- `1024x1024`
- `1536x1024`
- `1024x1536`
- `2048x2048`
- `2048x1152`
- `3840x2160`
- `2160x3840`
- `auto`

For explicit `gpt-image-2` dimensions, the maximum edge is 3840 pixels, both edges must be divisible by 16, the long-to-short ratio must not exceed 3:1, and total pixels must be from 655,360 through 8,294,400.

## Transparent backgrounds

For Codex/OpenAI, try `background: transparent` first. Google Flow rejects explicit background modes, so request a flat distinctive key color in the prompt instead. If the endpoint rejects transparency or the result is unsuitable, a local chroma-key fallback is available:

1. Generate the subject on a flat distinctive key color, named explicitly in the prompt.
2. Read `crux://skills/imagegen/scripts/remove_chroma_key.py` and copy it verbatim to `tmp/imagegen/remove_chroma_key.py`.
3. Run `python3 tmp/imagegen/remove_chroma_key.py --input <input> --out <output>` on the generated file to convert the key color to alpha. Do not modify the helper. Replace an existing output only with explicit consent.

If the subject cannot be extracted cleanly, explain the tradeoff before accepting an imperfect result.

## Output and limits

- The native client decodes bounded base64 responses and writes exactly the requested output count.
- Google Flow accepts only trusted Google HTTPS media URLs and rejects untrusted redirects before dispatch.
- Input images are limited to 16, 50 MiB each, and 200 MiB total.
- Each decoded or downloaded output is limited to 100 MiB; the complete API response is limited to 512 MiB.
- Output paths are validated before queueing and reserved against other active image jobs.
- Non-forced output writes use exclusive creation, so an external file created after queueing still cannot be silently overwritten.
- Large sizes, high quality, and multiple variants increase latency and cost.
- If an optional value is unsupported by the selected model, omit it only when doing so preserves the user's requested outcome.
