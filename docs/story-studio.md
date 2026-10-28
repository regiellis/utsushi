# Story studio (paused prototype)

The user restored the tool-led interface. Story routes are not registered and its CSS/JS
are not loaded. The code and stored planning are retained for reconsideration. The notes
below describe the dormant prototype, not the active interface.

The home route lists projects. An empty studio shows only the welcome creation form;
once a project exists, the sidebar offers New project. Each project has an episode board and a shared World.
An episode opens a workspace with scenes, a vertical media stage, shot direction, storyboard,
and planned-duration timeline. The sequence preview plays selected takes in order, skipping
shots without media. It does not render or export a finished episode.

## Storage and routes

`story_projects` stores each project as one JSON document. Episodes, shots, and world assets
have stable IDs inside it. Edits obtain the SQLite write lock before loading the document,
preventing concurrent forms from overwriting unrelated changes. Existing media and runs remain
in their original tables; planning references them by ID.

`story_web.go` owns project creation/editing, uploads, and views under `/projects`.
Every nested mutation checks ownership. Native POST forms include the framework's CSRF token.
Uploads reuse library validation and duplicate detection.

## Tools and media

Shot-aware `/motion`, `/swap`, and `/style` frames receive project, episode, and shot IDs.
They use the saved prompt and project visual direction; a chosen video can serve as source.
Submitted run IDs attach to the shot. A failure attaching them is logged without resubmitting
a billed generation; results remain in the global library.

Embedded tools permit same-origin frames only. Other security-policy directives remain intact.
Long-video and finishing tools share the library; their outputs are selected manually in the
shot inspector. World descriptions can be inserted into direction; images are selected in the
tool's existing reference picker. Dialogue is written planning, not synthesized audio.

Removing a shot keeps library media. Storage purge retains written projects and clears media
IDs and run references. Missing or separately deleted media renders an empty take.

## Interface

`story.tmpl`, `story.css`, and `story.js` own the story interface. Existing tool controls remain
available through Toolbox. The tour is always available; connection setup is offered on the first
visit without local/cloud execution. A user can dismiss it and continue planning.

The project format is vertical, but library files retain their original aspect ratio.
Blender integration, automatic continuity conditioning, soundtrack editing, and final episode
rendering are future work.
