# Optional Catwalk provider presets

This directory contains 26 canonical data-only provider presets migrated from Catwalk v0.51.23. They are optional migration aids for users moving from Crush to Crux and do not restore Catwalk as a runtime dependency or authority.

Each preset selects Crux's existing `openai-compat` Foundation implementation. Install one preset with:

```bash
crux plugins install ./plugins/provider-presets/deepseek.plugin
```

Install every preset from a repository checkout with:

```bash
for bundle in ./plugins/provider-presets/*.plugin; do
  crux plugins install --update "$bundle"
done
```

Restart Crux after installation so the provider catalog reloads. Installation validates and trusts the exact copied digest by default; use `--no-trust` to inspect without activation.

## Canonical compatible presets

| Provider | ID | Models |
| --- | --- | ---: |
| AIHubMix | `aihubmix` | 271 |
| Alibaba (Singapore) | `alibaba-singapore` | 16 |
| Alibaba (US) | `alibaba-us` | 3 |
| Atlas Cloud | `atlascloud` | 32 |
| Avian | `avian` | 11 |
| Baseten | `baseten` | 12 |
| Cerebras | `cerebras` | 2 |
| Chutes | `chutes` | 11 |
| DeepSeek | `deepseek` | 2 |
| Fireworks | `fireworks` | 14 |
| Groq | `groq` | 2 |
| Hugging Face | `huggingface` | 22 |
| io.net | `ionet` | 29 |
| Moonshot | `moonshot` | 11 |
| Nebius Token Factory | `nebius` | 24 |
| Neuralwatt | `neuralwatt` | 19 |
| OpenCode Go | `opencode-go` | 24 |
| OpenCode Zen | `opencode-zen` | 61 |
| QiniuCloud | `qiniucloud` | 14 |
| Scaleway | `scaleway` | 12 |
| Synthetic | `synthetic` | 10 |
| Venice AI | `venice` | 97 |
| xAI | `xai` | 6 |
| Z.AI | `zai` | 11 |
| Zhipu | `zhipu` | 10 |
| Zhipu Coding | `zhipu-coding` | 11 |

## Not representable as provider presets

These Catwalk records require runtime protocols or identity flows that the `provider-preset` contract cannot add. They are intentionally not relabeled as `openai-compat`. A separate runtime-backed provider bundle or host implementation is required.

| Provider | ID | Catwalk type | Reason | Models |
| --- | --- | --- | --- | ---: |
| Anthropic | `anthropic` | `anthropic` | Requires a runtime construction for Catwalk type `anthropic`; data-only presets cannot add it. | 14 |
| Azure OpenAI | `azure` | `azure` | Requires a runtime construction for Catwalk type `azure`; data-only presets cannot add it. | 14 |
| AWS Bedrock US | `bedrock` | `bedrock` | Requires a runtime construction for Catwalk type `bedrock`; data-only presets cannot add it. | 11 |
| AWS Bedrock Europe | `bedrock-europe` | `bedrock` | Requires a runtime construction for Catwalk type `bedrock`; data-only presets cannot add it. | 10 |
| GitHub Copilot | `copilot` | `openai-compat` | Crux owns Copilot through its specialized core construction; an ordinary preset would bypass its authentication and transport behavior. | 30 |
| Cortecs | `cortecs` | `openai` | Requires a runtime construction for Catwalk type `openai`; data-only presets cannot add it. | 96 |
| Google Gemini | `gemini` | `google` | Requires a runtime construction for Catwalk type `google`; data-only presets cannot add it. | 7 |
| Kimi Coding | `kimi-coding` | `anthropic` | Requires a runtime construction for Catwalk type `anthropic`; data-only presets cannot add it. | 2 |
| MiniMax | `minimax` | `anthropic` | Requires a runtime construction for Catwalk type `anthropic`; data-only presets cannot add it. | 8 |
| MiniMax China | `minimax-china` | `anthropic` | Requires a runtime construction for Catwalk type `anthropic`; data-only presets cannot add it. | 8 |
| OpenAI | `openai` | `openai` | Requires a runtime construction for Catwalk type `openai`; data-only presets cannot add it. | 28 |
| OpenRouter | `openrouter` | `openrouter` | Requires a runtime construction for Catwalk type `openrouter`; data-only presets cannot add it. | 260 |
| Vercel | `vercel` | `vercel` | Requires a runtime construction for Catwalk type `vercel`; data-only presets cannot add it. | 200 |
| Google Vertex AI | `vertexai` | `google-vertex` | Requires a runtime construction for Catwalk type `google-vertex`; data-only presets cannot add it. | 12 |

Catwalk v0.51.23 also contained a raw `pioneer.json` source record that was not registered in its provider inventory, so it was intentionally excluded from this canonical migration set.

## Reviewed differences from Catwalk

- Cerebras uses `X-Cerebras-3rd-Party-Integration: crux` instead of the inherited Crush value.
- Hugging Face uses `HTTP-Referer: https://github.com/example-git/crux` and `X-Title: Crux` instead of inherited Charm/Crush identity.
- AIHubMix keeps its opaque `APP-Code` provider-attribution value unchanged.
- Catwalk ignored direct `temperature` and `top_p` keys on the Cerebras `zai-glm-4.7` model because they are not fields in Catwalk's model type. The generated preset likewise omits those inactive keys rather than silently changing their meaning.

These checked-in manifests and their exact canonical digest bindings are the authoritative migration artifacts. Catwalk's MIT license is retained in `LICENSE.catwalk` and the repository's `THIRD_PARTY_NOTICES.md`.
