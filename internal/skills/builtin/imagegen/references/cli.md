# Native tool reference (`imagegen`)

Use the first-class `imagegen` agent tool directly. Do not invoke the hidden image command through Bash and do not write a script or SDK wrapper.

The tool queues work in Crux's native image pool and returns a task ID immediately. Four jobs may run concurrently; additional accepted jobs remain pending in FIFO order. Completion or failure is delivered automatically to the originating agent. A job with at least one successful variant completes and reports both saved outputs and failed variant details.

## Modes

- `generate`: create one or more images from a text prompt. `images` must be omitted.
- `edit`: create one or more images from ordered edit targets or visual references. At least one `images` path is required.

## Parameters

- `mode` (required): `generate` or `edit`
- `backend`: `auto` or `flow`; omitted means `auto`
- `prompt` (required): the image prompt
- `images`: edit-only ordered input paths, up to 16
- `model`: optional override; `auto` defaults to `gpt-image-2` for Codex or `gpt-image-1` for OpenAI, while `flow` defaults to `nano-banana-pro` and falls back once to `nano-banana-2` under the zero-output Pro conditions below
- `n`: number of outputs from 1 through 10; defaults to 1
- `quality`: `low`, `medium`, `high`, or `auto`
- `size`: `WIDTHxHEIGHT` or `auto`
- `background`: `transparent`, `opaque`, or `auto`
- `output`: one output file; valid only when `n` is 1
- `output_directory`: shared output directory using the next available numbered filenames; Google Flow uses `image_N.jpg` and other backends use `image_N.png`
- `force`: replace the exact resolved outputs when true

Exactly one of `output` and `output_directory` is required. Multiple jobs may use the same `output_directory`; without `force`, each job skips occupied or currently locked full output paths. Never set `force` without the user's explicit consent to replace exact paths.

## Generation example

```yaml
mode: generate
prompt: A cozy alpine cabin at dawn
size: 1024x1024
output: output/imagegen/alpine-cabin.png
```

## Google Flow example

```yaml
mode: generate
backend: flow
model: nano-banana-pro
prompt: A cozy alpine cabin at dawn
size: 1024x1024
output: output/imagegen/flow-alpine-cabin.jpg
```

Google Flow imports a usable local browser session automatically and retains it only in memory for the current Crux process. It uses Google Flow's direct non-Agent still-image operation and never falls back to Chat, Agent mode, video, or another backend. When Pro is unavailable before generation, or every Pro variant fails at the generation RPC before producing an image, Crux retries the complete zero-output attempt once with Nano Banana 2 when available. Partial Pro success and challenge, upload, response-validation, download, or cancellation failures do not trigger fallback, and Nano Banana 2 is not retried through another model. `size: auto` omits the aspect control; explicit dimensions must reduce to 1:1, 4:3, 3:4, 16:9, or 9:16. Jobs execute one at a time process-wide. Omit `quality` and `background`, or use `auto`.

## Edit example

```yaml
mode: edit
images:
  - input.png
prompt: Replace only the background with a warm sunset; preserve the subject and edges unchanged
output: output/imagegen/sunset-edit.png
```

## Reference-image example

```yaml
mode: edit
images:
  - references/editorial-lighting.png
prompt: Create a new ceramic mug product photo. Use Image 1 only as a lighting and composition reference; do not reproduce its subject.
output: output/imagegen/mug-reference-derived.png
```

## Variants example

```yaml
mode: generate
prompt: A product thumbnail of a matte ceramic mug on a stone surface
n: 4
output_directory: output/imagegen/mug-variants
```

Use `n` only for variants of the same prompt. Queue one native call per prompt and semantic output path for distinct assets.

## Task lifecycle

- The initial result confirms that the job was queued and includes its `i...` task ID.
- The job may be `pending` or `running` when the tool returns.
- Crux notifies the originating agent when the job completes, fails, is canceled, or is interrupted.
- Use `task_output` for explicit inspection and `task_stop` for cancellation.
- Report the readable result and final paths to the user after notification.

## Guardrails

- Existing outputs fail before generation unless `force` is true.
- Output paths are reserved across queued jobs to prevent two jobs targeting the same file.
- Input and output paths are canonicalized and permission checked.
- Input count, input bytes, response bytes, decoded image bytes, and output count are bounded.
- With `backend: auto`, if no supported account is available, ask the user to sign in with Codex or configure an OpenAI API account. Never ask for the key value in chat.
- With `backend: flow`, if no usable browser session is found, ask the user to sign in to Google Flow in a supported local browser and retry. Never ask for cookie values in chat.
- `flow` is an unofficial endpoint and never falls back to Codex/OpenAI.
