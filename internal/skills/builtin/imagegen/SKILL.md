---
name: imagegen
description: Generate or edit raster images when the task benefits from AI-created bitmap visuals such as photos, illustrations, textures, sprites, mockups, or transparent-background cutouts. Use when the agent should create a brand-new image, transform an existing image, or derive visual variants from references, and the output should be a bitmap asset rather than repo-native code or vector. Do not use when the task is better handled by editing existing SVG/vector/code-native assets, extending an established icon or logo system, or building the visual directly in HTML/CSS/canvas.
---

# Image Generation Skill

Use Crux's native `imagegen` tool to generate or edit raster images. The tool is available independently of this skill; this skill provides prompting and asset-placement guidance only.

The native tool owns authentication, API calls, output bounds, cancellation, persistence, and completion notification.

## Native execution contract

- Call `imagegen` with `mode: generate` when there are no image inputs.
- Call `imagegen` with `mode: edit` and one or more ordered `images` for edit targets or visual references.
- Omit `backend` or use `auto` for configured Codex/OpenAI selection. Use `backend: flow` for Google Flow's unofficial direct still-image path; it never falls back to Gemini Chat, Agent mode, video, or another backend.
- Exactly one of `output` or `output_directory` is required.
- `output` requires `n: 1`. Use `output_directory` for multiple variants; Google Flow uses the next available `image_N.jpg` filenames and other backends use `image_N.png`.
- The call queues the job and returns a task ID immediately.
- Up to four image jobs run concurrently. Google Flow jobs execute one at a time process-wide; additional accepted jobs remain pending.
- Completion or failure is delivered automatically to the originating agent. If only some variants succeed, the job completes with the saved paths and reports each failed variant. Use the returned result to report the final paths and any failures to the user.
- Use `task_output` to inspect a job only when the automatic notification is insufficient. Use `task_stop` to cancel it.
- Explicit `output` collisions are rejected unless `force` is true. Multiple jobs may share one `output_directory`; each job reserves distinct full output paths and skips occupied or currently locked numbered filenames. Set `force` only when the user explicitly consented to replacing exact paths.

With `backend: auto`, the native tool reuses configured provider accounts. It prefers the configured Codex account and otherwise uses the configured OpenAI API account. Default models are `gpt-image-2` for Codex authentication and `gpt-image-1` for OpenAI API authentication. If neither account is available, tell the user to sign in with Codex or configure an OpenAI API account. Never ask the user to paste an API key in chat.

With `backend: flow`, Crux automatically imports a Google session from supported local Chromium-family or Firefox profiles on macOS, Linux, and Windows, discovers an available Flow project and image models, and uses Flow's direct non-Agent still-image operation. Use `nano-banana-pro` (default) or `nano-banana-2`. When Pro is unavailable before generation, or every Pro variant fails at the generation RPC before producing an image, Crux retries the complete zero-output attempt once with Nano Banana 2 when that model is available. Partial Pro success and challenge, upload, response-validation, download, or cancellation failures never trigger fallback. Each variant is one independent direct image request, and Nano Banana 2 is never retried through another model. Edit inputs are uploaded once per Flow project and content hash, then securely reused only after Flow confirms the retained media remains available. Imported cookies and challenge tokens remain only in memory and are not written to configuration, accounts, task records, or the upload registry. If no usable session is found, tell the user to sign in to Google Flow in a supported browser and retry. Never ask the user to paste browser cookies in chat.

## When to use

- Generate a photo, illustration, texture, sprite, product shot, cover, banner, or website hero.
- Edit an existing raster image while preserving specified parts.
- Use one or more images as style, composition, mood, or subject references.
- Produce variants of one visual prompt.

## When not to use

- Extending or matching an existing SVG/vector icon set, logo system, or illustration library in the repository.
- Creating simple shapes, diagrams, wireframes, or icons better produced directly in SVG, HTML/CSS, or canvas.
- Making a small edit when an editable project-native source already exists.

## Workflow

1. Decide whether the request is generation or editing.
2. Decide whether the output is preview-only or belongs in the project.
3. Collect the prompt, exact text, constraints, avoid list, and input images.
4. Label each input image's role in the prompt: edit target, style reference, composition reference, or supporting input.
5. Shape the prompt into a concise production-oriented spec without inventing requirements.
6. Queue the native `imagegen` call with the intended output path.
7. For several distinct assets, queue one call per distinct prompt and path. Use `n` only for variants of the same prompt.
8. Continue any independent work while jobs run.
9. When completion notifications arrive, inspect the saved images and validate subject, style, composition, exact text, invariants, and avoid items.
10. Iterate with one targeted prompt change at a time when needed.
11. For project-bound work, keep the selected artifact in the workspace and update consuming code or references yourself.
12. Report final paths, prompts, model, quality, and any failures.

## Output-path policy

- Use `tmp/imagegen/` for preview-only or intermediate files.
- Put project-bound assets under the repository's established asset directory, or `output/imagegen/` when no convention exists.
- Do not treat a temporary preview as a finished project asset.
- Use sibling versioned names such as `hero-v2.png` instead of overwriting unless replacement was explicitly approved.

## Prompt shaping

Preserve detailed user prompts. For a generic prompt, add only details that materially improve the requested result, such as framing, polish level, intended use, or practical layout guidance.

Do not invent extra characters, objects, brands, slogans, palettes, narrative beats, or arbitrary placement.

Use this optional scaffold:

```text
Use case: <taxonomy slug>
Asset type: <where the asset will be used>
Primary request: <user's request>
Input images: <Image 1: role; Image 2: role>
Scene/backdrop: <environment>
Subject: <main subject>
Style/medium: <photo/illustration/3D/etc>
Composition/framing: <wide/close/top-down; placement>
Lighting/mood: <lighting and mood>
Color palette: <palette notes>
Materials/textures: <surface details>
Text (verbatim): "<exact text>"
Constraints: <must keep or avoid>
Avoid: <negative constraints>
```

For edits, repeat invariants every iteration: `change only X; keep Y unchanged`.

## Use-case taxonomy

Generate:
- `photorealistic-natural`
- `product-mockup`
- `ui-mockup`
- `infographic-diagram`
- `scientific-educational`
- `ads-marketing`
- `productivity-visual`
- `logo-brand`
- `illustration-story`
- `stylized-concept`
- `historical-scene`

Edit:
- `text-localization`
- `identity-preserve`
- `precise-object-edit`
- `lighting-weather`
- `background-extraction`
- `style-transfer`
- `compositing`
- `sketch-to-render`

## Model and quality guidance

- Use the backend-specific default model unless the user requests another model.
- Google Flow supports `nano-banana-pro` (default) and `nano-banana-2` when the selected account exposes their exact model usage. A zero-output Pro attempt falls back once to Nano Banana 2 only under the generation-specific conditions above. Omit `quality` and `background`, or use `auto`; explicit values fail because the direct operation does not execute them.
- For Google Flow, `size: auto` omits the aspect control. Explicit dimensions must reduce exactly to 1:1, 4:3, 3:4, 16:9, or 9:16 and select that aspect ratio; they do not guarantee exact output pixels.
- Use `quality: low` for fast Codex/OpenAI drafts, thumbnails, and quick iteration.
- Use `medium`, `high`, or `auto` for final assets, dense text, diagrams, identity-sensitive edits, or high-resolution outputs.
- Square images are usually fastest. `1024x1024` is a practical draft size.
- Common landscape and portrait sizes include `1536x1024`, `1024x1536`, `2048x1152`, `3840x2160`, and `2160x3840`.
- For `gpt-image-2`, non-auto dimensions must have edges divisible by 16, a maximum edge of 3840, a ratio no greater than 3:1, and total pixels from 655,360 through 8,294,400.

## Transparent backgrounds

For Codex/OpenAI, try `background: transparent` first. Google Flow rejects explicit background modes; request a flat chroma-key background in the prompt instead. If the selected endpoint does not honor transparency, use the chroma-key workflow in `references/image-api.md` or explain the tradeoff before accepting an imperfect result.

## References

- `references/cli.md`: native `imagegen` tool parameters and examples.
- `references/prompting.md`: detailed prompting principles.
- `references/sample-prompts.md`: reusable prompt recipes.
- `references/image-api.md`: model and request-shape context, including chroma-key fallback.
- `references/network.md`: authentication and permission behavior.
