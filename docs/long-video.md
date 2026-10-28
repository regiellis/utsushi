# Long video, runs in parts, and the clip editor

<!-- index -->
## Index

Sections by line range. Read with an offset to jump to one. Regenerate after edits.

- Contracts: lines 18-107
- Verification: lines 109-114
<!-- /index -->

This page holds the
contracts for making video longer than one generation: a clip run in parts (`parts.go`), the
Long video tab (`long_web.go`, `engine_long.go`, `longrun.go`, `sheets.go`,
`templates/pages/long.tmpl`, the `longvideo` script component), and the clip editor
(`clip_edit.go`, `templates/components/clipedit.tmpl`, `clipEditor` in the script).

## Contracts

- **Long video** (`long_web.go`, `engine_long.go`, `longrun.go`, `templates/pages/long.tmpl`,
  the `longvideo` script component) makes a video of shots, past any one model's length, on
  MiniMax H3's open reference-to-video model: ComfyUI's "MiniMax H3 Reference to Video" template
  (`workflows/minimax-h3-r2v.api.json`), on your ComfyUI or Comfy Cloud per Settings, Engines,
  "Open models run on", like the other template engines. A long video is a run of mode `long`
  whose `Parts` are its shots (`Prompt`, `Frames`, `Cut`, `TailID`), so cards, live updates,
  cancel, Collect, recover, Run again (`longAgain`, fresh shots), and delete come from the run
  machinery; `runs.source_id` is the start video, or NULL for a video started from a prompt
  (`sourceOptional` rebuilt the table once, by SQLite's documented procedure, since it was NOT
  NULL). A shot is 17k+5 frames at 24 fps inside the model's trained 124 to 362 (`longFrames`).
  The cast are its reference images (`<Picture N>` in the model's prompt, `longTags`). A shot
  that continues has the last 22 frames and their sound of what came before (`shotTail`,
  reversed, trimmed, and reversed back so it is exactly the last frames, at the run's size)
  anchored at frame 0 with `MiniMaxH3AddGuide`; it replays them first, so `joinShots` drops a
  continuing shot's first 22 frames and its sound with them, and concatenates every piece with
  its own sound (silence where it has none). A spike on the remote ComfyUI 2026-10-18 measured
  the step across that join at 28.97 dB PSNR against 27.70 dB for an ordinary frame step: no
  jump. The first shot of a video started from a prompt always starts fresh. The frame size is
  the start video's shape, or 9:16, 16:9, or 1:1, at 480 or 720 on the short side; Turbo runs
  the template's 4-step LoRA. A video started from a prompt has no Compare (nothing to compare
  against). Reuse on a long card opens the tab set up as that take (`longSetupOf`, `?reuse=`):
  cast in its order, start, shots, shape, size, quality, sheet view. Shots left as they were are
  kept (`keepShots`, each row naming its index in the earlier take, `shot_keep`) when they start a
  new shot or follow, directly, the shot they followed before, also kept: a continuing shot
  carries on from the very frames before it, so a change before a cut leaves the shots after it.
  Kept shots are copied as this take's own files (`copyMedia`, so deleting either
  take leaves the other whole), marked `Kept` (not counted again in spend), and only when the
  cast, start, size, quality, sheet view, and detail match too, since a shot kept under other settings
  would not match the shots made after it. A take that keeps every shot is refused. Character
  sheets go to the model as their first figure when the run's `cast_view` is "front" (the
  default; `frontFigure` finds a sheet by its figures on a plain background, not its shape,
  since sheets are as wide as landscape photos, and anything else goes whole): sent whole, a
  four-view sheet made MiniMax H3 draw extra copies of a person in a live run 2026-10-18.
  A shot can end on a library image (`part.EndID`): `endFrame` crops and scales it to the frame
  once, and a second `MiniMaxH3AddGuide` anchors it at `frame_idx` -1 (negative counts from the
  end), chained after the tail's guide. A spike 2026-10-18 measured the last frame at 30 dB PSNR
  against the image, with the shot moving toward it from a wide start. It is part of a shot, so a
  changed end image remakes it.
  A soundtrack (`runs.soundtrack`, a library audio file, and `soundtrack_mode`) is laid over the
  joined video by `soundtrackGraph`: "replace" makes it the sound, "mix" puts it under the shots'
  own at a third; either way padded or cut to the video's length. It is applied at the join, so
  kept shots do not depend on it, and a run's soundtrack keeps its file from deletion.
  `ref_size` sets the R2V node's `ref_image_size`: "match" (the template's, the default) scales
  the cast down to the video's area, "max" keeps up to 2048 px. With front views the cast is
  already smaller than the video's area, so the two send the same pixels: a same-seed pair of
  takes 2026-10-18 came out nearly frame for frame alike. It matters for whole sheets and large
  photos. What kept extra people out in that pair was the prompt: naming the count ("Only two
  people are in this shot"), the outfits, and the place. An earlier pair that seemed to favor
  "max" differed by seed.
- **Clip editor** (`clip_edit.go`, `templates/components/clipedit.tmpl`, `clipEditor` in the
  script), opened by Cuts on the clip card and the cut icon on reference video and long video
  start tiles: the clip with its cuts over the scrub strip, detected (`frames.cuts`) or as saved.
  Save keeps them in `media.cuts` ('' until saved, so runs detect; '[]' means no cuts; Reset
  forgets them), and `startRuns` splits a run in parts at saved cuts before detected ones. Split
  makes 2 or 3 new library clips at the cuts (`splitClip`: each at least `clipShortest`,
  re-encoded so each starts on its first frame, the clip itself kept) and adds them to whichever
  lists the page shows by out-of-band swaps (`clips-split`). Trim (`trimClip`, Start here and End
  here at the playhead, I and O) makes one new clip of a stretch at least `clipShortest` long. `cleanCuts` keeps cuts inside the
  clip and apart, `clipEdge` from an end or each other.
- **Long clips run in parts** (`parts.go`). A clip past the engine's `MaxSeconds`, under
  `longClipStop` (31 s, so a "30 s" clip that measures 30.04 s still counts), is planned at run
  creation and stored in `runs.parts`. Less than `trimOver` (1 s) past the limit, it is trimmed
  to the limit less `partMargin`: one generation, no join, no second bill. Otherwise `planParts`
  makes the fewest equal segments under the limit less `partMargin`, each sharing `partOverlap`
  (0.5 s) with the next, and `atCuts` moves each boundary onto the nearest of the clip's own
  cuts (`frames.cuts`, ffmpeg scene score over 0.3) that keeps every part between the engine's
  shortest clip or output and its limit, keeping the count, so the studio's mirror
  (`partsFor`) still totals right. A part that starts on a cut opens on a fresh shot and shares
  nothing with the one before. That matters: the first live run split mid-shot, Wan 3.0 skipped
  a cut inside a part, and a performer dropped out. `attemptParts` cuts each segment (libx264,
  `originPart` media, hidden from the library), runs them one after another with the same
  inputs, one seed (`runSeed`, from the run's key, through `task.Seed`, so versions still
  differ), and an idempotency key per part (`-p1`, `-p2`). From the second part on, the
  previous part's last moment (`partFrame`) joins the references and the prompt names it as the
  same people, when the engine reads tags and has room for an image: the cast carries forward.
  A Style preset run takes its style frame the same way. `joinGraph` fits every part to its
  segment's length (a speed factor from the result's own length), `joinFPS`, and the first
  part's size, joins straight where parts meet at a cut and crossfades with `xfade` where they
  overlap, and lays the clip's own sound over the whole, so the result runs as long as the clip
  (or the trimmed span). Generated audio from the parts is dropped: the clip's sound stays in
  step. The part in flight carries its request id on the part and on the run (`partTracker`),
  so recover and Collect resume it; `heldRequest` is what cancel asks the provider about, and
  nothing between parts. Each part bills (`estimateUSD`, `runner.billed`), and a run that fails
  partway records what its finished parts cost. Checked end to end with real ffmpeg and an
  engine that echoes its segment (`TestALongClipRunsInPartsAndJoinsBackToItsLengthWithItsSound`,
  `TestAClipRunsInPartsSplitAtItsCutAndCarriesTheCastForward`). Live 2026-10-18, Wan 3.0 on a
  30 s clip before cuts, the seed, and the carried frame: the cast held their sheet outfits once
  the prompt named them, but a mid-shot crossfade ghosted and a performer dropped at a cut.

## Verification

```bash
task test   # includes the end-to-end parts, long video, soundtrack, and split tests (real ffmpeg)
task eval   # long video shot prompts are in evals/prompts.json
```
