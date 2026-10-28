# Tools

<!-- index -->
## Index

Sections by line range. Read with an offset to jump to one. Regenerate after edits.

- Contracts: lines 36-187
  - Depth maps: lines 97-123
  - Pose: lines 125-143
  - Edges: lines 145-151
  - Person matte: lines 153-160
  - Upscale: lines 162-168
  - Frame interpolation: lines 170-177
  - Clip tools (ffmpeg alone): lines 179-187
- Speed: lines 189-201
- Verification: lines 203-212
<!-- /index -->

This page holds the
contracts for the Tools page and its nine tools, all of which make library media from one library
item on this machine, free, without ComfyUI.

| File | Holds |
| --- | --- |
| `tools.go` | the `tool` registry (`toolList`), `makeTool`, the job (`toolJob`: notes, scratch files, `keep`, model downloads) |
| `tools_web.go`, `templates/pages/tools.tmpl`, `tools` in `static/js/app.js` | the page, its form, the cards |
| `tool_depth.go`, `depth.go`, `depth_render.go` | depth maps |
| `tool_pose.go`, `image_ops.go` | pose, and the OpenCV-style resize, warp, and drawing it needs |
| `tool_edges.go` | edges (Canny) |
| `tool_models.go` | person matte, upscale, frame interpolation |
| `tool_clip.go` | grab a frame, conform, extract audio |
| `onnx_models.go`, `onnx_net.go` | pinned model files and downloads, the generic ONNX session |
| `ffmpeg.go` | `rate`, `rawFrames`, `encodeRaw`, `cutout` |

## Contracts

- **A tool is one entry in `toolList`**: what it takes (`Takes`), whether it runs a model
  (`Model`), `plan` (reads the form, names the items it makes, or refuses with a `failure` the
  form shows), and `run`. Everything else is shared. `makeTool` adds one pending library item per
  output (`origin = 'tool'`, `media.tool` the tool's id, `media.source_id` the source, no foreign
  key so deleting a source leaves what was made from it) and runs the job in the background.
  Model tools run one at a time (`toolKit.heavy`), since each keeps every core busy; the clip
  tools start at once. Progress notes go to each item's `media-<id>` channel and end in "· 42%"
  for the card's bar. A restart fails a pending item, as it does images. Depth maps had an origin
  of their own first; `newStore` moves those rows over.
- **Retry.** Each item keeps its order's options (`media.tool_form`, the posted form less the tool
  and source, `orderForm`). Retry on a failed card (`retryTool`, `POST /tools/items/{id}/retry`)
  plans the same order again, finds this item among its outputs by label, and remakes only it,
  in place: the other outputs are placeholders that `keep` discards. Depth items made before
  options were kept read theirs back from the label (`tool.formOf`). An item whose source is
  gone, or that has not failed, is refused with a toast.
- **The page** is `/tools?tool=<id>`. The tool picker is the engine picker's row and popover.
  The source pickers are the studio's: `clip-block` (from `pageClip`, so the clip is the one
  every studio tab remembers, and picking one here changes it there) and the reference flyout's
  image panel. The form is `#run-form` because the clip block's hidden `source` input names that
  form. `source_kind` says which pane is live, and an image goes in `image`, one at a time. The
  feed shows everything of origin `tool`, newest first.
- **Outputs line up with their source.** Every tool but Conform, Upscale, and Grab a frame keeps
  the source's size (even sides for H.264) and its average frame rate, and carries its sound. An
  image source makes a PNG at its size.
- **Frame rate** comes from ffprobe's `avg_frame_rate` (`pickRate`). ffprobe prints fields in
  its own order whatever order is asked for, and a phone clip read `r_frame_rate` 60/1 over
  24 fps, which made a 2 s map play in 0.8 s. Decoding passes frames through as they are
  (`-fps_mode passthrough`), so the count matches the source.
- **Models run in process** through ONNX Runtime (`yalue/onnxruntime_go` v1.36.0, which asks for
  C API 29, served by the ONNX Runtime 1.31.0 library it loads; both checked 2026-10-18). The
  bindings use cgo, so Utsushi builds with cgo on; the proxy stays `CGO_ENABLED=0`. Linux x64
  only (`modelPlatform`). Files download on first use into `<data>/models`, each pinned by URL
  with a commit or release in it and by SHA-256, hashed before it is renamed into place, so a
  changed upstream file is refused. An archive (`.tgz`, or a `.whl`, which is a zip) keeps only
  its listed `Members`, flat in the file's `Dir`.
- **The GPU** (`onnx_device.go`). `findDevice` picks this machine's NVIDIA GPU when the driver's
  `/dev/nvidiactl` exists and `nvidia-smi` names a GPU, else the CPU; `UTSUSHI_TOOLS_DEVICE=cpu`
  forces the CPU. The startup log and the Tools page say which. On the GPU the runtime is ORT
  1.31.0's CUDA 13 build (ORT's default since 1.27; driver 580 is the CUDA 13.0 branch) with
  NVIDIA's own PyPI wheels for the libraries its CUDA provider needs: CUDA runtime 13.4, cuBLAS
  13.8, cuRAND 10.4, cuDNN 9.27 for CUDA 13 (`runtimeFiles`, about 1.2 GB to download, 1.9 GB
  unpacked in `<data>/models/cuda`), so nothing is installed on the system. The provider links
  cudart, cuBLAS, cuBLASLt, and cuRAND by name and opens cuDNN by name, with no runpath of its
  own, and LD_LIBRARY_PATH is read only when a process starts, so `startORT` opens each of
  `cudaPreload` by full path with `RTLD_GLOBAL` (cgo `dlopen`) before loading ORT; the NVIDIA
  libraries find their siblings through `$ORIGIN`. Every session takes `sessionOptions`, which
  attaches the CUDA provider; one that will not attach leaves the session on the CPU, logged
  once. A model job that runs out of GPU memory (`gpuFull`: the provider's arena or CUDA refusing
  an allocation) runs again on the CPU (`sessionsOnCPU`, safe because model jobs run one at a
  time), its card saying so and naming who holds the GPU (`gpuHolders`, from nvidia-smi). Another
  program can hold most of the card: a local ComfyUI kept 15.5 of the 20 GB on 2026-10-18, and
  Stable depth's head failed asking for 1.93 GB more; with the retry the same job finished on the
  CPU. One process loads one ORT library, so the device is fixed at the first model job.
  Checked 2026-10-18 on the RTX 4000 SFF Ada (20 GB): the GPU's depth matched the CPU's at
  49.9 dB PSNR, the card ran at 95% with 1.4 GB in use, and every model tool ran several times
  faster (the table below). Every model's weights are permissive (Apache-2.0, MIT,
  BSD-3-Clause). The Base and Large depth models are CC-BY-NC and RMBG is non-commercial, so they
  stay out.

### Depth maps

- **Stable** (`videoDepth`) ports `infer_video_depth` from Video Depth Anything's
  `video_depth.py` and `utils/util.py`: windows of 32 stepping 22, a window's first 10 inputs the
  previous window's inputs at keyframes 0, 12, 24..31 (so input 0 is always frame 0), a
  least-squares scale and shift over two keyframes onto the reference, the 8 frames windows
  share blended linearly, negatives clamped, the clip padded with its last frame. Each frame is
  encoded once. The export (TheoreticallyPose's split of VDA Small) is fixed at 518×910, so
  frames are fitted inside it and edge-padded (`planar`), and the padding is cropped off
  (`cropPlane`). `TestVideoDepthAlignsEveryWindowOntoTheFirst` checks the window inputs and that
  per-window scale and shift come out.
- **Sharp** (`frameDepth`) runs Depth Anything V2 Small per frame at its own size (`dav2Size`:
  short side 518, multiples of 14, lower bound, as the model's transform does). It is the
  default.
- **Rendering** goes through a float file of every frame's depth, so the range is known first
  (`depthRange`, each frame's 0.5th and 99.5th percentiles). Stable spans the whole clip; Sharp
  takes each frame's own span averaged over ±4 frames (`depthSmooth`). Sharp first chained a
  least-squares fit of each frame onto the one before: regression dilution shrank the scale a
  little each step and left the middle of a 2 s clip mid-gray. Its depth holds only up to a
  scale and shift per frame, so a per-frame span is the honest one. The models give relative
  disparity (near is larger): Depth is near-white, Inverted near-black, Normals a Sobel slope of
  the levels (`normalStrength`) as OpenGL RGB, flat facing the camera (128, 128, 255).
- **Measured** 2026-10-18 on this CPU (24 cores): Sharp about 0.4 s a frame, Stable about 1 s
  (a 10 s 30 fps portrait clip: 117 s and 296 s). On the two portrait clips tried, Stable was
  not measurably steadier than Sharp (frame-to-frame change in a wall patch 2.4% against 2.2% of
  contrast, floor 1.7% against 2.0%) and was softer: a 9:16 frame gets 291×518 of its fixed
  input.

### Pose

- Ports ControlNet's DWPose annotator (DWPose branch `onnx`, commit 3dca5db,
  `annotator/dwpose`). Poses are found and drawn at 512 on the short side (`poseDraw`), the
  annotator's usual detect resolution, then scaled to the source, so limbs keep the width the
  control models were trained on.
- YOLOX-L gets the RGB frame scaled into the top left of a 640 square of gray 114, raw 0..255.
  Rows run stride 8, 16, 32; objectness and class scores are already sigmoided in the graph; a
  person is objectness times class 0 over 0.3, then greedy NMS at IoU 0.45 with the +1 pixel
  convention. No person found means the whole frame is one box.
- dw-ll_ucoco_384 gets each box padded by 1.25, set to 3:4, warped to 288×384 (bilinear, black
  border), and normalized with ImageNet's 0..255 mean and std. SimCC is decoded by argmax over
  each axis, split ratio 2, the score the smaller of the two maxima.
- The 133 COCO-WholeBody points map to OpenPose's 18 (`openposeFromCOCO`); the neck is the
  shoulders' middle, seen only when both are. Points under 0.3 are left out. Drawing follows
  `draw_pose`: 17 limbs as filled ellipses 4 px wide in OpenPose's 18 colors, the whole canvas
  dimmed to 0.6, joints as radius 4 discs, then hands (edges 2 px in HSV hues, points blue),
  then face points white, radius 3. Feet are not drawn. About 0.25 s a frame on this CPU with
  one person (2 s, 48 frames: 33 s including the 351 MB download).

### Edges

- `canny` is OpenCV's Canny as ControlNet's annotator calls it (`cv2.Canny(img, low, high)` on
  the RGB frame): a 3 by 3 Sobel per channel with edges repeated, the channel with the largest L1
  gradient per pixel, OpenCV's four-direction non-maximum test (tan 22.5° in 15-bit fixed point,
  13573), and hysteresis over the eight neighbors. Default 100 and 200 is ControlNet's; Fine is
  50 and 100, Bold 150 and 300. White edges on black at the source's size.

### Person matte

- MODNet (Xenova's ONNX of the official export), sized as its own `inference_onnx.py` sizes it
  (`modnetSize`: ref 512, then multiples of 32), input `(px - 127.5) / 127.5`, output alpha
  0..1. Matte is the alpha as gray at the source's size. Cutout multiplies the source by the
  matte in ffmpeg (`frames.cutout`), both inputs' timestamps reset and the output at the
  source's rate: without that, blend mixed the two time bases and a 24 fps clip came out at
  2880/119. MODNet is trained on portraits and mattes each frame alone.

### Upscale

- Real-ESRGAN's compact x4 models: realesr-general-x4v3 for live action, realesr-animevideov3
  for drawn footage. Input RGB 0..1, any size; output clamped. 2× is the x4 output scaled to half,
  as Real-ESRGAN's own outscale does. The result may be at most 3840 on its long side
  (`upscaleLongest`); past that the form says to conform smaller first. About 3.2 s a frame for a
  720×1280 source on this CPU (2 s, 48 frames: 153 s).

### Frame interpolation

- RIFE 4.9 (FuryTMP's ONNX, MIT per Practical-RIFE; its exporter is unknown). Input is 7
  channels: frame A RGB, frame B RGB, both 0..1 and padded right and bottom with black to
  multiples of 32, then a plane of the time t. 2× inserts t = 0.5, 4× t = 0.25, 0.5, 0.75. Across
  a cut (`frameSSIM` of 32 by 32 gray thumbnails under 0.2, Practical-RIFE's threshold) the frame
  repeats instead. The last frame holds for the factor, so the clip keeps its length and its
  sound lines up: 48 frames at 24 fps became 96 at 48.

### Clip tools (ffmpeg alone)

- **Grab a frame** keeps one frame as a PNG at the clip's size (`frameAt` keeps the time a frame
  inside the clip). The page's preview seeks a `<video>` to the slider.
- **Conform** crops to 9:16, 16:9, or 1:1 from the center, scales the short side to 480, 720, or
  1080, and sets 16, 24, 25, or 30 fps (`conformFilter`), ending on even sides. It refuses an order
  that changes nothing.
- **Extract audio** keeps the sound as 48 kHz WAV, or MP3 past `audioWAVSeconds` (150 s), since
  the library takes audio up to 30 MB. A clip with no sound is refused.

## Speed

Measured 2026-10-18, whole jobs through the page (decode, model, encode), CPU on 24 cores against
the RTX 4000 SFF Ada:

| Tool, clip | CPU | GPU |
| --- | --- | --- |
| Depth Stable, 10 s 30 fps 480×854 | 296 s | 16 s |
| Depth Sharp, same clip | 117 s | 16 s |
| Pose, 2 s 24 fps 720×1280 | about 12 s (33 s with the model download) | 4 s |
| Person matte, same clip | 6 s | 2 s |
| Upscale 2×, same clip | 153 s | 26 s |
| Frame interpolation 2×, same clip | 16 s | 4 s |

## Verification

```bash
task test   # windowing, spans, Canny, pose mapping and drawing, RIFE layout, the clip tools end to end on an ffmpeg-made clip, the form's refusals, the cards
```

A live check runs each tool through the page on a real clip, then compares a frame of the source
and the output side by side with ffmpeg and checks with ffprobe that the output has the frames
and rate it should. Done 2026-10-18 for all nine on `drive2s.mp4` (720×1280, 24 fps, 2 s) and depth
on a 10 s portrait clip.
