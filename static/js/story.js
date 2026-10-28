// Episode UI: dialogs, saved-shot tools, and playback of selected takes.
(() => {
  const setup = document.querySelector('[data-story-first-setup]');
  if (setup) {
    try {
      if (!localStorage.getItem('utsushi-story-setup-seen')) {
        setup.showModal();
        setup.addEventListener('close', () => localStorage.setItem('utsushi-story-setup-seen', '1'), {once: true});
      }
    } catch (error) { /* Browsing remains available when storage is blocked. */ }
  }
  let dirty = false;
  let playing = false;
  let timer;
  let playback;
  const editor = document.querySelector('[data-story-editor]');
  editor?.addEventListener('input', () => {
    dirty = true;
    document.querySelector('[data-story-save-status]').textContent = 'Unsaved direction. Save before opening a tool.';
  });
  editor?.addEventListener('submit', () => { dirty = false; });
  window.addEventListener('beforeunload', (event) => {
    if (dirty) { event.preventDefault(); event.returnValue = ''; }
  });
  function stop() {
    playing = false;
    clearTimeout(timer);
    playback?.pause?.();
    document.querySelector('[data-story-play]')?.replaceChildren(document.createTextNode('▶ Preview sequence'));
  }
  function preview() {
    if (playing) { stop(); return; }
    const clips = [...document.querySelectorAll('[data-story-clip]')].filter(x => x.dataset.storyClip);
    const stage = document.querySelector('[data-story-preview]');
    const status = document.querySelector('[data-story-play-status]');
    if (!stage || !status) return;
    if (!clips.length) { status.textContent = 'Choose and save a take for at least one shot first.'; return; }
    playing = true;
    document.querySelector('[data-story-play]').textContent = '■ Stop preview';
    let index = 0;
    function next() {
      if (!playing) return;
      clearTimeout(timer);
      playback?.pause?.();
      if (index >= clips.length) { stop(); status.textContent = 'End of sequence.'; return; }
      const item = clips[index++];
      const seconds = Number(item.dataset.storySeconds);
      const video = item.dataset.storyKind === 'video';
      const el = document.createElement(video ? 'video' : 'img');
      el.src = item.dataset.storyClip;
      status.textContent = `${index} / ${clips.length} · ${item.dataset.storyTitle}`;
      let advanced = false;
      const advance = () => { if (!advanced && playing) { advanced = true; next(); } };
      el.addEventListener('error', () => { status.textContent = `Unable to preview ${item.dataset.storyTitle}. Select another take.`; stop(); });
      if (video) {
        el.playsInline = true;
        el.controls = true;
        el.addEventListener('ended', advance);
        el.addEventListener('timeupdate', () => { if (el.currentTime >= seconds) advance(); });
      } else {
        el.alt = item.dataset.storyTitle;
        el.addEventListener('load', () => { timer = setTimeout(advance, seconds * 1000); }, {once: true});
      }
      stage.replaceChildren(el);
      playback = el;
      if (video) el.play().catch(() => { status.textContent = 'Press play on the video to continue the sequence.'; });
    }
    next();
  }
  document.addEventListener('click', (event) => {
    const open = event.target.closest('[data-story-open]');
    if (open) document.getElementById(open.dataset.storyOpen)?.showModal();
    if (event.target.closest('[data-story-close]')) event.target.closest('dialog')?.close();
    if (event.target.closest('[data-story-tour]')) document.getElementById('story-tour')?.showModal();
    const insert = event.target.closest('[data-story-insert]');
    if (insert && editor) {
      const input = editor.elements.prompt;
      input.value += (input.value ? '\n' : '') + insert.dataset.storyInsert;
      input.dispatchEvent(new Event('input', {bubbles: true}));
      input.focus();
    }
    const tool = event.target.closest('[data-story-tool]');
    if (tool) {
      if (dirty) { editor.querySelector('button[type=submit], button:not([type])')?.focus(); return; }
      stop();
      const dialog = document.getElementById('story-tool-dialog');
      dialog.querySelector('iframe').src = tool.dataset.storyTool;
      dialog.showModal();
    }
    if (event.target.closest('[data-story-tool-close]')) document.getElementById('story-tool-dialog')?.close();
    if (event.target.closest('[data-story-play]')) preview();
  });
  document.querySelector('[data-story-delete]')?.addEventListener('submit', (event) => {
    const form = event.currentTarget;
    if (form.dataset.confirmed) { dirty = false; return; }
    event.preventDefault();
    const dialog = document.createElement('dialog');
    dialog.className = 'story-dialog story-form';
    const text = document.createElement('p');
    text.textContent = 'Remove this shot from the episode? Generated media stays in the library.';
    const cancel = document.createElement('button');
    cancel.className = 'btn btn-secondary'; cancel.textContent = 'Keep shot';
    cancel.addEventListener('click', () => dialog.close());
    const remove = document.createElement('button');
    remove.className = 'btn btn-confirm is-danger'; remove.textContent = 'Remove shot';
    remove.addEventListener('click', () => { form.dataset.confirmed = 'true'; dialog.close(); form.requestSubmit(); });
    dialog.append(text, cancel, remove);
    dialog.addEventListener('close', () => dialog.remove(), {once:true});
    document.body.append(dialog); dialog.showModal(); cancel.focus();
  });
})();
