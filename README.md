# Utsushi

<!-- index -->
## Index

Sections by line range. Read with an offset to jump to one. Regenerate after edits.

- Run it: lines 50-82
- Engines and cost: lines 84-148
  - Clip editor: lines 128-134
  - Clips longer than an engine takes: lines 136-148
- Long video: lines 150-172
- Settings: lines 174-220
- Create an image: lines 222-251
- Wan Animate 2: lines 253-269
- Wan 3.0 Reference to Video: lines 271-291
- Style: lines 293-302
- Tools: lines 304-333
- Restarts, lost contact, and errors: lines 335-349
- Development: lines 351-356
- Model tiers and featured models: lines 358-386
<!-- /index -->

A video studio for your own ComfyUI, built on the Go SDK and the `comfy-api-proxy` binary from
[comfy-api-proxy-go](https://github.com/regiellis/comfy-api-proxy-go). It recreates the workflow
of Higgsfield's Genjutsu:

- **Motion Transfer**: give it a clip and reference images. It recasts the people and keeps the
  motion, camera, and timing.
- **Object Swap**: give it a clip and a prompt (and optionally a reference). It replaces one thing
  and keeps the rest.

Tools makes depth, pose, edge, and matte videos from a clip on this machine, for driving a run
with motion alone, and upscales, interpolates, conforms, and pulls frames and sound from clips.

Wan Animate 2 and reference images run free on your own GPU, through the local `comfy-api-proxy`.
References can also be uploaded. Every clip and image previews on hover, a finished result opens
full size from its card, and Compare opens a split viewer with Source and Result side by side.

Finished results are downloaded into a local library, and each one opens in a before/after
slider.

It is a [HyperChi](https://hyperchi.regiellis.com/docs) app: server-rendered pages, htmx 4, and
server-sent events for live progress.

> [!IMPORTANT]
> Independent project. Not affiliated with or endorsed by Comfy Org or Higgsfield. Comfy and
> ComfyUI are trademarks of Comfy Org. Genjutsu and Higgsfield are Higgsfield's.

## Run it

```bash
task run                  # installs the comfy-api-proxy release into bin/, then runs the studio with Air
```

Air must be on `PATH` ([installation](https://github.com/air-verse/air#installation)).
It rebuilds and restarts the studio when Go code, templates, CSS, JavaScript,
workflow files, or SDK code change. Runtime files under `data/` are excluded.
Build output stays in `.cache/utsushi-air/` at the repository root.
Ctrl+C stops Air and the studio.

It serves on port 8080, or the next free port, and prints every address it answers on, including
the LAN one. Set `HOST`/`PORT` to change that.

Utsushi starts `comfy-api-proxy` on 127.0.0.1:8189 itself, fronting the ComfyUI chosen in
Settings, restarts it if it dies, and stops it on exit. A proxy you started yourself on that port
is used as it is, and Settings then cannot move it. It finds the binary at `UTSUSHI_PROXY_BIN`,
then `comfy-api-proxy` on `PATH`, then `bin/comfy-api-proxy` (where `task proxy` installs it).

Without a Comfy API key the page still loads, and Wan Animate 2 and local references still work.
The billed engines stay out of the picker until a key is added under Settings.

| Variable | Default | |
| --- | --- | --- |
| `COMFY_API_KEY` | (none) | used until a key is saved in Settings; read from `.env` by `task run` (template `.env.example`) |
| `UTSUSHI_DATA` | `data` | SQLite file, the media library, the preview cache, crash reports |
| `UTSUSHI_CONFIG` | `~/.config/utsushi/settings.toml` | the Settings file |
| `UTSUSHI_PROXY_URL` | `http://127.0.0.1:8189` | set it elsewhere to use a proxy Utsushi does not manage |
| `UTSUSHI_PROXY_TOKEN` | (none) | that proxy's bearer token, if it was started with one |
| `UTSUSHI_PROXY_BIN` | (search) | the `comfy-api-proxy` binary to run |
| `UTSUSHI_ANIMATE_WORKFLOW` | `workflows/wan-animate2.api.json` | the Wan Animate 2 export |
| `COMFY_CLOUD_URL` | `https://cloud.comfy.org` | the v2 deployment for uploads |

## Engines and cost

| Mode | Engine | Runs on | Billed | Limits |
| --- | --- | --- | --- | --- |
| Motion Transfer | Genjutsu Motion Transfer | Comfy Router | yes, per version | clip 4 to 30 s, 1 to 8 references, 480p/720p/1080p |
| Motion Transfer | Wan Animate 2 | your ComfyUI via the proxy, or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | clip 1 to 30 s, 1 reference, 480p/720p |
| Motion Transfer | Wan 3.0 Reference to Video | partner node on the local ComfyUI | yes, per second of output | clip 2 to 15 s, 1 to 10 images, up to 4 videos and 5 audio clips, 480P/720P/1080P |
| Motion Transfer | Seedance 2.5 | Comfy Cloud | yes, per second of output and of the clip | clip 2 to 30 s, 1 to 9 images, up to 2 videos and 3 audio clips, 480p/720p/1080p |
| Motion Transfer | MiniMax H3, H3 Max | Comfy Cloud | yes, per second of output, plus the clip | clip 2 to 15 s (output 4 to 15 s), 1 to 9 images, up to 2 videos and 3 audio clips, 768P/2K |
| Motion Transfer | MiniMax H3 Pose | your ComfyUI via the proxy, or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | clip 2 to 15 s, 1 to 9 images, 480p/720p |
| Motion Transfer | MiniMax H3 Control | your ComfyUI via the proxy, or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | a depth, pose, or edge clip from Tools, 2 to 15 s, 1 to 9 images, 480p/720p |
| Motion Transfer | LTX-2.3 Control | your ComfyUI via the proxy, or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | a depth, pose, or edge clip from Tools, 2 to 20 s, 1 opening image, 480p/720p |
| Style | Genjutsu, Wan 3.0, Seedance 2.5 Edit, Wan 2.7 Edit, Aleph 2 | as above, after a free local step | yes, as above | the same clip limits; characters optional |
| Style | LTX-2.3 IC-LoRA | your ComfyUI via the proxy, or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | clip 2 to 20 s, 480p/720p |
| Object Swap | Runway Aleph 2 | Comfy Router | yes, per version | clip up to 30 s, prompt, 0 or 1 reference |
| Object Swap | Wan 2.7 Video Edit | Comfy Router | yes, per version | clip 2 to 10 s, prompt, up to 4 references, 720P/1080P |
| Object Swap | Seedance 2.5 Edit | Comfy Cloud | yes, per second of output and of the clip | clip 2 to 30 s, prompt, up to 9 images, 480p/720p/1080p |

Seedance and MiniMax bill the clip they are given as well as the output, so their estimates count
both; a 10 s clip at 720p comes to about $3.95 on Seedance 2.5. Their price chips say "+ clip".
Seedance 2.5 and MiniMax H3 run as partner nodes on Comfy Cloud, so they need only the Comfy API key.
MiniMax H3 Pose and LTX-2.3 IC-LoRA are ComfyUI's own templates ("MiniMax H3 Fun ControlNet Union",
"LTX 2.3 IC-LoRA Union Control"), run on your ComfyUI, which needs their models; each engine says
which file is missing. MiniMax H3 Pose drives MiniMax H3 with the clip's poses and your picked
images. LTX-2.3 takes the clip's depth and the restyled first frame, so it serves Style mode.
MiniMax H3 Control and LTX-2.3 Control run the same two templates on a depth, pose, or edge clip
you made on the Tools page, picked as the source clip, in place of the control the template would
make itself; MiniMax H3 Control takes your cast images, LTX-2.3 Control one opening frame. Like Wan Animate 2 and the open image
models, they have separate **Your ComfyUI** and **Comfy Cloud** choices. Local runs are free;
Cloud bills GPU time with no published price, so the studio asks before each run.
New video runs retain their chosen destination across Settings changes and restarts.
For cloud workflow Style presets, both the reference-frame preparation and video run on
Comfy Cloud and use billed GPU time. Upload a style image to skip frame generation.
If preparation is interrupted, check Cloud history before retrying; that step may have billed.
Cloud workflow cards remain visible without a key, with a setup warning; the explicit
“Hide engines that aren’t ready” setting still filters unavailable choices.

On a billed engine, each version is a separate generation and a separate charge. The Generate button asks before it
starts. Router does not always report credits used for queued runs, so the card shows a cost only
when Router sends one.

Uploads go to your Comfy Cloud asset store, because Router models fetch their inputs from a public
URL. The app passes them the signed link Comfy Cloud issues.

### Clip editor

Cuts on the clip card (and the scissors on reference videos and long video start tiles) opens
the clip with its shot changes marked on a timeline. Add a cut at the playhead (C), click a cut
to remove it, or go back to the detected ones. Save cuts, and a clip that runs in parts splits
there. Split turns the clip into 2 or 3 new clips in your library at the cuts, keeping the
original. Start here and End here (I and O) mark a stretch, and Trim makes it a new clip.

### Clips longer than an engine takes

A clip longer than the engine's limit, up to 30 s (anything under 31 s, since a 30 s clip often
measures a little over), runs in parts instead of being refused. A clip less than a second over
is simply trimmed to the limit, one generation. Otherwise the clip is cut into the fewest parts
the engine takes, at the clip's own shot cuts where they fit, so each part starts on a fresh shot
and the joins fall where the clip already cuts. Where no cut fits, neighboring parts overlap by
half a second and crossfade. The engine runs the parts one after another with the same prompt,
references, and seed, and from the second part on it also sees the last moment of the part
before as one more reference, so the cast stays the same people. ffmpeg then joins them, each
part fitted to its length, with the clip's own sound over the whole, so the result runs as long
as the clip. Each part is a separate generation, so a billed engine bills each one; the Generate
total adds them up. This needs ffmpeg on the server.

## Long video

The Long video tab makes a video of shots, as long as you need, with MiniMax H3's open model on
your ComfyUI (free) or on Comfy Cloud, as Settings, Engines, "Default for open models" says. Pick a
cast (character sheets hold a person best), then write the shots: each has its own prompt and a
length of 5 to 15 s, and either continues from the shot before it or cuts to a new one. A shot
that continues picks up from the last second of the one before, motion and sound included,
instead of cutting. Start from a prompt, or pick a library video to extend it. Shots render one
after another and join into one video with each shot's own sound. Character sheets go to the
model as their front figure by default, because a whole sheet (one person shown four times) can
come out as several people. Full detail sends the cast at up to 2048 px instead of the
video's size; with front views it changes little, since each figure is already that small. Say
how many people are in each shot and what they wear: that is what kept strangers out of wide
shots in our tests. Reuse on a long video's card opens the tab set up as that take:
change a shot and Generate, and the shots you left alone are kept, not made again, except a shot
that continues one you changed.
A shot can end on an image from your library ("Ends on"): its last frame lands on that
picture, so you can steer where a shot finishes. A soundtrack from your library can play over the whole video, replacing the shots' own sound
or mixed under it. With a language model on, each shot has Improve and Suggest: they know the cast, the shot
before, and whether this one continues it. Draft at 480p with Turbo (4
steps) first; 720p and Full (20 steps) take longer. Your ComfyUI needs the MiniMax H3 R2V files:
`minimax_h3_ref2va_pruned_int8_convrot`, `qwen3vl_32b_minimax_h3_nvfp4_awq`, the video and audio
VAEs, and `minimax_h3_ref2v_turbo_4step_v0.1_comfyui_bf16` for Turbo.

## Settings

The gear in the top bar opens Settings. Everything is saved to one TOML file
(`~/.config/utsushi/settings.toml`, mode 0600, folder 0700), keys included. A typo in that file
stops startup rather than being replaced by defaults on the next save.

- **ComfyUI server**: this machine or a remote one (a LAN box, a rented GPU), with an optional
  access token. Test connection reads the server directly and shows its version, GPU, queue, and
  whether each feature's nodes and model files are there. Save only goes through when that test
  passes, then restarts the proxy pointed at it. The Comfy API key lives here too.
- **Language model**: optional. Claude, OpenAI, llama.cpp, Ollama, LM Studio, or any
  OpenAI-compatible endpoint, reached through `chat/completions`. It powers three buttons by the
  prompt. Improve rewrites your prompt and Suggest drafts one; both look at three frames of the
  clip, each picked reference by its @tag, and the style. An engine that reads tags gets every
  picked tag back; for the others the references are described in words. Describe writes the
  first reference's look (Wan Animate gets its two-line format). Undo puts the old prompt back.
  All three need a vision model.
- **Engines and queue**: turn engines off, pick each mode's default, hide unready ones, the default
  resolution, how many local runs share the GPU at once (others wait for a slot), and browser
  notifications when a run ends.
- **Spending**: show estimates, ask before a run over $1, $2, or $5, and a monthly cap that either
  asks or blocks billed engines. Estimates and the cap use published rates only, so they count
  Wan 3.0; Router engines have no published price and are not counted.
- **Storage**: copy each finished result to an output folder, named by a pattern
  (`{date} {time} {mode} {engine} {style} {v} {id}`), keep or delete source clips with their run,
  delete finished runs after 30 or 90 days, and clear the preview cache. **Purge library storage**
  requires typing `PURGE` exactly. It permanently removes registered library media, run history,
  remembered selections, and previews, resetting recorded spending totals. Pending work and
  unresolved runs block it. Settings, keys, downloaded models, and exported copies remain.
- **About**: versions, Copy diagnostics (versions, server, settings with keys masked, as text), and
  crash reports. Reports are always written to `data/crash/`; they are sent only when sending is on
  and an endpoint is set.

With ffmpeg on the server, video cards get a poster and a hover scrub strip, cached in
`data/cache/`, and Suggest takes its frames with it. Without ffmpeg, cards fall back to the video
itself and Suggest takes the first frame through ComfyUI.

To use another ComfyUI, choose Remote server and enter its address. Utsushi finds model files by
name on that server, wherever they sit under `models/`, and an engine whose model is missing there
says which file in its card. The top bar shows that machine's GPU.

Source and reference pickers in the studio and Tools have filename search and 24-item
pages. Their controls stay visible while the tiles scroll, and paging keeps your picked
references. Older files remain available beyond the former 60-video / 120-image cutoff.
Clear selection empties the source slot; Delete from library removes the file after
confirmation, unless a run uses it. Re-uploading identical content or choosing Use as
source again reuses the existing library copy.

## Create an image

"Create an image" in the reference flyout opens a dialog for making references: a character
sheet (four views of one character), an environment sheet (a wide establishing view over three
smaller views of the same place), an object, a place, or anything. Describe it, pick a model, and
the dialog writes the prompt from your choices in the way that model's makers recommend, so the
same request reads differently for Qwen, Krea, Nano Banana, or Seedream. You can edit it before
you create. Images
land in the reference library as they finish.

| Model | Runs on | Price per image | References |
| --- | --- | --- | --- |
| Z-Image Turbo | your ComfyUI or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | none |
| Krea-2 Turbo | your ComfyUI or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | none |
| Qwen Image 2.1, 2.1 Turbo | your ComfyUI or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | none |
| Qwen Image 2.1 Edit | your ComfyUI or Comfy Cloud | free on your GPU; billed by Comfy Cloud otherwise | 1 to 4 |
| Nano Banana 2.1 | Comfy Cloud | $0.059 at 1K, $0.083 at 2K | up to 14 |
| Nano Banana 2 | Comfy Cloud | $0.084 at 1K, $0.122 at 2K | up to 14 |
| Krea 3 | Comfy Cloud | $0.043 at 1K plus $0.029 per reference | up to 8 |
| Seedream 5.0 Pro | Comfy Cloud | $0.045 at 1K, $0.09 at 2K, plus $0.003 per extra reference | up to 10 |
| GPT Image 2.5 Flare, Sunburst | Comfy Cloud | high quality: $0.0753 square, $0.0589 portrait or landscape, $0.0606 wide or tall, plus $0.0117 to $0.0176 per reference | up to 16 |

Models are ranked for what you are making, from our own test runs: ●●● did best, ●●○ was good,
●○○ fell short, and ○○○ has not been tried for that yet. Each card says what its run showed, and
the dialog starts on the best one that can run. The engine picker ranks video engines per mode
the same way. A ranking is one or two runs, not a benchmark.

Prices are the ones each partner node publishes. Comfy Cloud bills open models by GPU time and
publishes no per-image price, so the dialog asks before running one there. Choose the local or cloud card for each open image model. Settings supplies the default
for older selections and for Long video.

## Wan Animate 2

This engine runs ComfyUI's built-in "Wan Animate 2" template (`video_wan_animate2`) on the local
ComfyUI, through the proxy. ComfyUI needs the template's models: `wan_animate_2_int8_convrot`,
the `lightx2v_I2V_14B_480p` LoRA, `umt5_xxl_fp8_e4m3fn_scaled`, `clip_vision_h`, and
`Wan2_1_VAE_bf16`.

- `workflows/wan-animate2.api.json` is the template in API format, exported unchanged from the
  ComfyUI frontend (`app.graphToPrompt()`, which is what Export (API) runs).
- `workflows/wan-animate2.bind.json` names the inputs Utsushi fills, as `node_id.input_name`: the
  clip, the reference, the prompt, the sampler seed (fresh per version), the frame width and height
  (from the clip's aspect at the chosen short side), and `output`, the node whose video is kept.
  The template's side-by-side comparison video is not kept, since the run page has its own.

The prompt describes the character's look and the background, not the motion. The template loops
over 81-frame windows, so the result has every frame of the clip. A 2 s, 48-frame clip at 480p took
about 6 minutes on this machine.

## Wan 3.0 Reference to Video

This engine runs the `Wan3ReferenceToVideoApi` partner node on the local ComfyUI, through the proxy.
The node calls Comfy's partner API and bills your Comfy credits through your Comfy API key (Settings, or `COMFY_API_KEY`), which the
app sends with the job as `extra_data.api_key_comfy_org`. Per second of output, the node's price
table says $0.0715 at 480P, $0.143 at 720P, and $0.286 at 1080P.

Besides images, Wan 3.0 takes reference videos (for motion or a scene) and audio (WAV or MP3,
for voice, music, or sound), picked from tabs in the reference flyout. In the studio prompt they
are `@1` for images, `@v1` for videos, and `@a1` for audio, each counted on its own; Utsushi
sends them as `@Image1`, `@Video2` onward (the clip is `@Video1`), and `@Audio1`. The clip and
the reference videos may run 15 s together, and the audio 15 s in all.

The clip goes in as `@Video1` and the references as `@Image1` onward. With no prompt, Utsushi
writes one in the shape the Wan 3.0 prompt guide recommends: the recast first, then each reference
by role, audio direction, and the resolution last. A prompt that already uses `@` tags is sent as
written. The output is as long as the clip. Clips stop at 15 s, because clip and output together
may not pass 30 s.

In a live run, Wan 3.0 kept the clip's setting, camera, and broad motion, and recast the performer.
It did not follow the clip frame for frame, as Wan Animate 2 does.

## Style

Style mode restyles a whole clip: pick one of 19 looks, or your own image, and every frame takes
it while motion, camera, and framing stay as filmed. Characters are optional. When you press
Generate, Utsushi first restyles the clip's first frame on your GPU with Qwen Image 2.1 Edit (free,
about a minute or two), then sends that frame to the engine as the style, with a prompt to restyle
the whole clip to match. This needs the local proxy and these models in ComfyUI:
`qwen_image_2.1_int8_convrot`, `qwen3vl_8b_int8_convrot`, and `qwen_image_2.1_vae_bf16`.

The preset tiles are samples made once with Z-Image on the first visit to Style mode, one at a time.

## Tools

Tools, in the top bar, makes library media from one clip or image on this machine, free, without
ComfyUI. Pick a tool, a source (the studio's own clip, or an image from its library), and the
tool's options. What it makes lands in the library under its source's name, so any mode can pick
it as its clip, a reference, or a soundtrack.

| Tool | Makes | Takes |
| --- | --- | --- |
| Depth maps | depth (near white), inverted, or a normal map | clip or image |
| Pose | OpenPose skeletons with hands and face, as DWPose draws them | clip or image |
| Edges | Canny outlines, white on black | clip or image |
| Person matte | a mask of the people, or them cut out over black | clip or image |
| Grab a frame | one frame as an image | clip |
| Conform | the clip at 16 to 30 fps, 480p to 1080p, or 9:16, 16:9, 1:1 | clip |
| Extract audio | the clip's sound, WAV or MP3 | clip |
| Upscale | 2× or 4× with Real-ESRGAN, up to 3840 on the long side | clip or image |
| Frame interpolation | 2× or 4× the frames with RIFE, same length | clip |

The model tools run on this machine's NVIDIA GPU when it has one, through ONNX Runtime's CUDA
provider, and on the CPU otherwise (`UTSUSHI_TOOLS_DEVICE=cpu` forces it). On the RTX 4000 SFF
Ada, a 10 s portrait clip's depth takes 16 s (117 s on the CPU) and a 2× upscale of a 2 s
720×1280 clip 26 s (153 s); `docs/tools.md` has the rest. Edges, Conform, Grab a frame, and
Extract audio take seconds either way.

The first run of a model tool downloads ONNX Runtime (about 1.2 GB with the CUDA libraries, once)
and that model into `data/models`, each checked against a pinned SHA-256; the picker says how
much. Nothing is installed on the system. Model tools run one at a time, since each keeps every
core busy. Tools need ffmpeg and ffprobe, and the model tools Linux x64. Details are in
`docs/tools.md`.

## Restarts, lost contact, and errors

Every run stores its provider's request or job id as soon as the provider accepts it. If the
server stops while a run is queued or generating, the next start reconnects and collects it, so a
billed result is not lost. A run that had not reached a provider is marked interrupted, and nothing
was billed for it.

If the app loses contact with a provider that holds a run (a dropped connection, a 5xx, rate
limiting), it tries to collect again for about two and a half minutes. After that the card reads
"Connection lost" with a Collect button, and the next start tries again too. A provider's own
refusal or failure is shown as the provider wrote it, and a ComfyUI node failure names the node.
A failed, canceled, or interrupted run can be started again with the same inputs from its card.

Cards stay current across tabs: a tab's live updates pause while it is hidden, and it catches up
when it is shown again.

## Development

```bash
task test     # tests against a fake Router queue, nothing is billed
task lint
```

## Model tiers and featured models

MiniMax H3 and LTX video engines appear first in the picker. FLUX 3 from Black Forest Labs
has a featured section in Create image, using Comfy Cloud's `Flux3ImageNode` with up to ten
references (at least 256 × 256 pixels), at 1K or 2K. Its schema and price badge were checked
against the connected ComfyUI's `/object_info` on 2026-10-22: $0.06864 at 1K, $0.143 at 2K.
Grounding is off. These integrations have workflow tests; featuring them is not a quality
endorsement, and FLUX 3 has not yet been graded with a billed live generation.

Local tiers describe estimated workflow budgets: Low (12 GB), Mid (16–20 GB), High (24–32 GB),
and Workstation (over 32 GB). `model_tiers.go` records provisional planning budgets, including
2–8 GB for references and working memory. These are not measured minimums. The current
shipped profiles start at 16 GB; no 12 GB workflow is claimed as validated. Quantization,
offloading, resolution, duration, reference sizes, and concurrent work change actual memory use.
Z-Image's vendor describes a 16 GB target; other budgets are conservative app planning estimates,
not vendor guarantees. Unprofiled models remain explicitly unverified.

Hardware comes from the execution server's ComfyUI `/system_stats`, refreshed after 30 seconds,
not the browser or the Utsushi host. The first non-CPU device is used, matching the existing
runtime display; VRAM from multiple devices is never added together. Unknown hardware and
shared-memory devices are not classified as a definite fit. Low total or currently free VRAM
dims the card with a warning; selecting it overrides the advice without bypassing missing-model,
missing-node, input-validation, or billing checks. Cached models can make free VRAM look low.

Cloud bands compare published unit prices: Budget up to $0.05, Standard up to $0.15, and Premium
cost above that, separately per image and per output second. Budget becomes Value only with a
Good or Best task rating. Quality is from the existing task-specific test runs, not inferred from
price or featured placement. Input-video fees, resolution and reference charges still use the full
cost estimate; GPU-time models and unpublished rates remain price unknown.
