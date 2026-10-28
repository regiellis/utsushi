// Page components for the directives runtime. Server state stays on the
// server; these only handle presentation: a toast, media previews, the
// studio's live totals, the feed filter, and the result viewer.

// ok reports whether an htmx request event ended below 400.
HyperChi.registerComponent('ok', (event) => {
  const status = event?.detail?.ctx?.response?.status;
  return typeof status === 'number' && status < 400;
});

// Re-uploading a file returns its existing library item. Keep one tile per
// picker, preserving a checked tile before the component rebuilds its tags.
function reconcileLibraryTiles() {
  document.querySelectorAll('.cast, #styles').forEach((grid) => {
    const seen = new Map();
    for (const tile of [...grid.children]) {
      const id = tile.dataset.libraryId;
      if (!id) continue;
      const previous = seen.get(id);
      if (!previous) { seen.set(id, tile); continue; }
      if (tile.querySelector('input:checked')) {
        previous.remove();
        seen.set(id, tile);
      } else {
        tile.remove();
      }
    }
  });
}
document.addEventListener('htmx:after:swap', reconcileLibraryTiles, true);

// A video can be shown as both a source and a reference on the same page.
// Remove every representation after the response has finished swapping.
document.addEventListener('media-deleted', (event) => {
  const id = event.detail?.id;
  if (!id) return;
  requestAnimationFrame(() => {
    document.querySelectorAll('[data-library-id]').forEach((tile) => {
      if (tile.dataset.libraryId === id) tile.remove();
    });
    const source = document.querySelector('#clip-block input[name="source"]');
    if (source?.value === id) document.querySelector('[data-clear-clip]')?.click();
    document.querySelector('.studio')?.dispatchEvent(new Event('change', { bubbles: true }));
  });
});

// Paged library views retain hidden checkboxes so changing a page or search
// never changes the references selected for a run.
const libraryBrowsers = new WeakMap();
function watchLibraries() {
  document.querySelectorAll('.pick-scroll > .cast, .pick-scroll > .clip-grid').forEach((grid) => {
    if (libraryBrowsers.has(grid)) { libraryBrowsers.get(grid)(); return; }
    const scroll = grid.parentElement;
    const search = document.createElement('label');
    search.className = 'library-search';
    search.innerHTML = '<span class="sr-only">Search library by name</span><input type="search" placeholder="Search by name…" autocomplete="off">';
    scroll.before(search);
    const paging = document.createElement('div');
    paging.className = 'library-paging';
    paging.innerHTML = '<span class="meta" role="status" aria-live="polite"></span><button type="button" class="btn btn-secondary btn-small" aria-label="Previous page">Previous</button><button type="button" class="btn btn-secondary btn-small" aria-label="Next page">Next</button>';
    scroll.after(paging);
    const empty = document.createElement('p');
    empty.className = 'library-no-match';
    empty.textContent = 'No matching files. Try another name.';
    empty.hidden = true;
    scroll.append(empty);
    const input = search.querySelector('input');
    const [prev, next] = paging.querySelectorAll('button');
    let page = 0;
    const size = 24;
    const refresh = () => {
      const tiles = [...grid.children].filter((tile) => tile.dataset.libraryId);
      const query = input.value.trim().toLocaleLowerCase();
      const matches = tiles.filter((tile) => (tile.dataset.libraryLabel || '').toLocaleLowerCase().includes(query));
      const pages = Math.max(1, Math.ceil(matches.length / size));
      page = Math.min(page, pages - 1);
      const visible = new Set(matches.slice(page * size, (page + 1) * size));
      tiles.forEach((tile) => {
        tile.hidden = !visible.has(tile);
        tile.querySelectorAll('video').forEach((video) => { video.preload = tile.hidden ? 'none' : 'metadata'; });
      });
      const start = matches.length ? page * size + 1 : 0;
      const end = Math.min((page + 1) * size, matches.length);
      paging.querySelector('[role="status"]').textContent = query
        ? `${start}-${end} of ${matches.length} matches`
        : `${start}-${end} of ${tiles.length} files`;
      prev.disabled = page === 0;
      next.disabled = page + 1 >= pages;
      empty.hidden = !query || matches.length > 0;
      if (grid.classList.contains('bento')) packBento(grid);
    };
    input.addEventListener('input', () => { page = 0; scroll.scrollTop = 0; refresh(); });
    prev.addEventListener('click', () => { page = Math.max(0, page - 1); scroll.scrollTop = 0; refresh(); });
    next.addEventListener('click', () => { page++; scroll.scrollTop = 0; refresh(); });
    new MutationObserver(refresh).observe(grid, { childList: true });
    libraryBrowsers.set(grid, refresh);
    refresh();
  });
}
document.addEventListener('DOMContentLoaded', watchLibraries);
document.addEventListener('htmx:after:swap', watchLibraries, true);

// media builds the element a preview shows: a muted looping video or an
// image.
function media(kind, src, controls) {
  const el = document.createElement(kind === 'video' ? 'video' : 'img');
  if (kind === 'video') {
    el.muted = !controls;
    el.loop = true;
    el.playsInline = true;
    el.autoplay = true;
    el.controls = Boolean(controls);
  } else {
    el.alt = '';
  }
  el.src = src;
  return el;
}

// place puts the hover preview beside its trigger: to the right when it
// fits, else the left, clamped inside the viewport.
function place(box, trigger) {
  const r = trigger.getBoundingClientRect();
  const w = box.offsetWidth;
  const h = box.offsetHeight;
  const gap = 12;
  let left = r.right + gap;
  if (left + w > window.innerWidth - gap) left = r.left - gap - w;
  left = Math.max(gap, Math.min(left, window.innerWidth - gap - w));
  const top = Math.max(gap, Math.min(r.top + r.height / 2 - h / 2, window.innerHeight - gap - h));
  box.style.left = `${left}px`;
  box.style.top = `${top}px`;
}

// bentoLayout packs tiles into a true bento: two-row bands, each filled
// with whole blocks, so no cell is ever empty. A block is a 2x2 feature, a
// 1x2 tall tile, two 2x1 wides stacked, a wide over two smalls, or two
// smalls stacked. shapes are the server's tile shapes in order ("wide",
// "tall", or "" for square-ish). Every other band opens on a feature,
// alternating sides. When the last band comes up short, a few tiles spread
// through the grid become features and it packs again, since a 2x2 crops
// a tile far less than stretching the last band would; stretching from
// the right is the last resort. It returns each tile's cell: x, y, w, h.
function bentoLayout(shapes, cols) {
  if (cols < 2) return shapes.map((_, i) => ({ x: 0, y: i, w: 1, h: 1 }));
  const promoted = new Set();
  let best = null;
  for (let tries = 0; tries < 8; tries += 1) {
    const plan = bentoBands(shapes, cols, promoted);
    if (!best || plan.short < best.short) best = plan;
    if (!plan.short) break;
    // Promote tiles spread evenly over the ones still plain, enough to
    // cover the shortfall.
    const plain = plan.plain.filter((i) => !promoted.has(i));
    if (!plain.length) break;
    for (let k = 0; k < Math.min(plan.short, plain.length); k += 1) {
      promoted.add(plain[Math.floor(((k + 0.5) * plain.length) / Math.min(plan.short, plain.length))]);
    }
  }
  return bentoFlush(best, cols);
}

// bentoBands lays tiles out band by band; promoted tiles take a feature
// block. It returns the bands' blocks, how many columns the last band
// lacks, and the tiles in one-column blocks (the ones a feature would
// widen).
function bentoBands(shapes, cols, promoted) {
  const queue = shapes.map((_, i) => i);
  // take removes the first tile within reach whose shape is wanted.
  const take = (want, reach = 4) => {
    for (let k = 0; k < Math.min(reach, queue.length); k += 1) {
      if (!promoted.has(queue[k]) && want.includes(shapes[queue[k]])) return queue.splice(k, 1)[0];
    }
    return -1;
  };
  const tile = (i, x, y, w, h) => ({ i, x, y, w, h });
  const bands = [];
  const plain = [];
  let used = 0;
  for (let band = 0; queue.length; band += 1) {
    const blocks = [];
    used = 0;
    while (used < cols && queue.length) {
      const room = cols - used;
      const i = queue.shift();
      const s = shapes[i];
      let block;
      if (room >= 2 && cols >= 4 && (promoted.has(i) || (!blocks.length && band % 2 === 0))) {
        block = { w: 2, tiles: [tile(i, 0, 0, 2, 2)] };
      } else if (s === 'tall') {
        block = { w: 1, tiles: [tile(i, 0, 0, 1, 2)] };
        plain.push(i);
      } else if (s === 'wide' && room >= 2) {
        const j = take(['wide']);
        const k = j < 0 ? take(['', 'wide']) : -1;
        const l = k >= 0 ? take(['', 'wide']) : -1;
        if (j >= 0) block = { w: 2, tiles: [tile(i, 0, 0, 2, 1), tile(j, 0, 1, 2, 1)] };
        else if (l >= 0) block = { w: 2, tiles: [tile(i, 0, 0, 2, 1), tile(k, 0, 1, 1, 1), tile(l, 1, 1, 1, 1)] };
        else if (k >= 0) block = { w: 2, tiles: [tile(i, 0, 0, 2, 1), tile(k, 0, 1, 2, 1)] };
        else block = { w: 2, tiles: [tile(i, 0, 0, 2, 2)] };
      } else {
        const j = take(['', 'wide']);
        block = j >= 0
          ? { w: 1, tiles: [tile(i, 0, 0, 1, 1), tile(j, 0, 1, 1, 1)] }
          : { w: 1, tiles: [tile(i, 0, 0, 1, 2)] };
        plain.push(i);
      }
      used += block.w;
      blocks.push(block);
    }
    bands.push(blocks);
  }
  return { bands, short: cols - used, plain };
}

// bentoFlush widens a short last band's blocks from the right until it is
// flush, then places every tile, every fourth band mirrored so features
// alternate sides.
function bentoFlush(plan, cols) {
  const at = [];
  plan.bands.forEach((blocks, band) => {
    let used = blocks.reduce((n, blk) => n + blk.w, 0);
    for (let b = blocks.length - 1; used < cols; b = (b - 1 + blocks.length) % blocks.length) {
      const blk = blocks[b];
      blk.tiles.forEach((t) => { if (t.x + t.w === blk.w) t.w += 1; });
      blk.w += 1;
      used += 1;
    }
    const row = band % 4 === 2 ? [...blocks].reverse() : blocks;
    let x = 0;
    row.forEach((blk) => {
      blk.tiles.forEach((t) => { at[t.i] = { x: x + t.x, y: band * 2 + t.y, w: t.w, h: t.h }; });
      x += blk.w;
    });
  });
  return at;
}

// packBento lays a .bento grid's visible tiles out with bentoLayout. The
// column count follows the grid's width, data-bento-cell being the
// smallest cell, and rows are as tall as columns are wide.
function packBento(grid) {
  const width = grid.clientWidth;
  if (!width) return;
  const gap = parseFloat(getComputedStyle(grid).columnGap) || 0;
  const cell = Number(grid.dataset.bentoCell || 120);
  const cols = Math.max(1, Math.floor((width + gap) / (cell + gap)));
  const side = (width - gap * (cols - 1)) / cols;
  const tiles = [...grid.children].filter((el) => getComputedStyle(el).display !== 'none');
  const at = bentoLayout(tiles.map((el) => el.dataset.shape || ''), cols);
  grid.style.gridTemplateColumns = `repeat(${cols}, minmax(0, 1fr))`;
  grid.style.gridAutoRows = `${side}px`;
  tiles.forEach((el, n) => {
    const p = at[n];
    el.style.gridColumn = `${p.x + 1} / span ${p.w}`;
    el.style.gridRow = `${p.y + 1} / span ${p.h}`;
  });
}

// watchBentos packs every .bento grid on the page, and again whenever its
// tiles change, its feed filter changes, or its width does. Grids inside a
// closed flyout have no width and pack when it opens.
const bentos = new WeakSet();
function watchBentos() {
  document.querySelectorAll('.bento').forEach((grid) => {
    if (bentos.has(grid)) return;
    bentos.add(grid);
    let width = -1;
    const pack = () => {
      width = grid.clientWidth;
      packBento(grid);
    };
    new ResizeObserver(() => { if (grid.clientWidth !== width) pack(); }).observe(grid);
    new MutationObserver(pack).observe(grid, { childList: true, attributes: true, attributeFilter: ['data-filter'] });
    pack();
  });
}

// shell is the page-wide component on <body>. Besides the toast, it owns
// the hover preview and the lightbox, delegated from the body so cards and
// tiles swapped in over SSE need no wiring of their own.
HyperChi.registerComponent('shell', () => {
  let current = null; // the [data-peek] element under the pointer
  let player = null; // the audio reference playing, if any
  let wait = 0;
  let pending = null; // resolves the open confirm dialog
  let savedSettings = false; // a Settings save happened since the page loaded
  const notified = new Set(); // run cards already announced
  const hidePeek = (box) => {
    clearTimeout(wait);
    current = null;
    box.classList.remove('is-open');
    box.replaceChildren();
  };
  return {
    message: '',
    timer: 0,
    // init wires the Create image dialog. Its listeners capture, so the
    // reference order and the edited flag are in the form before htmx sends
    // the preview that the same change triggers.
    init() {
      watchBentos();
      document.addEventListener('change', (event) => {
        const ref = event.target.closest?.('[data-image-ref]');
        if (ref) this.imageRefs(ref);
        const purpose = event.target.closest?.('[data-image-purpose]');
        if (purpose) {
          // A new purpose brings its own default shape and hint.
          document.querySelectorAll('#image-form input[name="aspect"]').forEach((a) => { a.checked = false; });
          const box = document.getElementById('image-subject');
          if (box) box.placeholder = purpose.dataset.hint;
          const hint = document.querySelector('[data-image-hint]');
          if (hint) hint.textContent = purpose.dataset.hint;
        }
      }, true);
      document.addEventListener('input', (event) => {
        if (event.target.closest?.('[data-image-prompt]')) {
          const touched = document.querySelector('[data-image-touched]');
          if (touched) touched.value = '1';
        }
      });
      document.addEventListener('image-made', () => {
        this.closeImages();
        this.toast('Making your image. It appears in References when it is ready.');
        const picker = document.querySelector('.ref-picker');
        if (picker) picker.open = true;
      });
      // A closed dialog empties, or the next swap anywhere would open it
      // again: swapped opens any dialog that has content. $refs is not set
      // yet while init runs (reading it threw here, so no dialog ever
      // emptied), and close does not bubble, so this listens on the
      // document while the event captures.
      document.addEventListener('close', (event) => {
        if (event.target.id === 'clip-editor' || event.target.id === 'image-dialog') event.target.replaceChildren();
      }, true);
    },
    // imageRefs keeps the dialog's picked references in pick order, as
    // hidden refs inputs the preview and Create both send.
    imageRefs(box) {
      const form = document.getElementById('image-form');
      const holder = form?.querySelector('[data-image-refs]');
      if (!holder) return;
      let ids = [...holder.querySelectorAll('input')].map((i) => i.value);
      ids = ids.filter((id) => id !== box.dataset.imageRef);
      if (box.checked) ids.push(box.dataset.imageRef);
      holder.replaceChildren(...ids.map((id) => Object.assign(document.createElement('input'), { type: 'hidden', name: 'refs', value: id })));
    },
    closeImages() {
      const dlg = this.$refs.images;
      if (dlg?.open) dlg.close();
      dlg?.replaceChildren();
    },
    // clipEdited closes the clip editor after a save or a split and says
    // what happened.
    clipEdited(message) {
      const dlg = this.$refs.clipEditor;
      if (dlg?.open) dlg.close();
      dlg?.replaceChildren();
      this.toast(message);
    },
    toast(text) {
      this.message = String(text || '');
      clearTimeout(this.timer);
      this.timer = setTimeout(() => { this.message = ''; }, 6000);
    },
    // An error mark shows its message in the error tip while the pointer or
    // keyboard focus is on it; a tap toggles it, since a touch has no hover.
    showError(mark) {
      const tip = this.$refs.errorTip;
      this.$refs.errorTitle.textContent = mark.dataset.error || '';
      this.$refs.errorHint.textContent = mark.dataset.errorHint || '';
      tip.classList.add('is-open');
      place(tip, mark);
      tip.dataset.for = mark.dataset.error || '';
    },
    hideError() {
      this.$refs.errorTip.classList.remove('is-open');
    },
    // Previews follow a mouse or keyboard focus. A touch has no hover, and
    // its tap already selects or opens the media.
    peekIn(event) {
      const mark = event.target.closest?.('[data-error]');
      if (mark) {
        if (event.pointerType === undefined || event.pointerType === 'mouse') this.showError(mark);
        return;
      }
      if (event.pointerType !== undefined && event.pointerType !== 'mouse') return;
      const trigger = event.target.closest?.('[data-peek]');
      if (!trigger || trigger === current) return;
      const box = this.$refs.peek;
      hidePeek(box);
      current = trigger;
      wait = setTimeout(() => {
        if (current !== trigger || !trigger.isConnected) return;
        const el = media(trigger.dataset.peek, trigger.dataset.peekSrc, false);
        const show = () => {
          if (current !== trigger) return;
          place(box, trigger);
          box.classList.add('is-open');
        };
        el.addEventListener(trigger.dataset.peek === 'video' ? 'loadeddata' : 'load', show, { once: true });
        box.replaceChildren(el);
      }, 160);
    },
    peekOut(event) {
      const mark = event.target.closest?.('[data-error]');
      if (mark && !(event.relatedTarget instanceof Node && mark.contains(event.relatedTarget))) this.hideError();
      if (!current) return;
      const to = event.relatedTarget;
      if (to instanceof Node && current.contains(to)) return;
      hidePeek(this.$refs.peek);
    },
    openLightbox(event) {
      const trigger = event.target.closest?.('[data-lightbox]');
      if (!trigger) return;
      event.preventDefault();
      hidePeek(this.$refs.peek);
      const src = trigger.dataset.lightboxSrc;
      this.$refs.lightboxStage.replaceChildren(media(trigger.dataset.lightbox, src, true));
      this.$refs.lightboxDownload.href = src;
      const compare = this.$refs.lightboxCompare;
      compare.hidden = !trigger.dataset.lightboxRun;
      compare.href = trigger.dataset.lightboxRun ? `/runs/${trigger.dataset.lightboxRun}` : '#';
      this.$refs.lightbox.showModal();
    },
    // addRun shows a run started elsewhere (another tab, Run again) at the
    // top of the feed. The tab that started it usually has the card from
    // its own response, so presence is checked before and after the fetch.
    addRun(event) {
      const id = String(event.detail?.data || '');
      const feed = document.getElementById('runs');
      if (!/^[a-z0-9]+$/.test(id) || !feed || document.getElementById(`run-${id}`)) return;
      fetch(`/runs/${id}/card`, { headers: { 'HX-Request': 'true' } })
        .then((res) => (res.ok ? res.text() : ''))
        .then((html) => {
          if (!html || document.getElementById(`run-${id}`)) return;
          feed.insertAdjacentHTML('afterbegin', html);
          htmx.process(feed.firstElementChild);
        })
        .catch(() => {});
    },
    // A hidden tab's SSE streams pause, and what was broadcast meanwhile is
    // gone. The server sends "connected" on every (re)connect, so each live
    // element refetches itself after a reconnect. Still live: only its body
    // is replaced, keeping the stream. Finished: the whole element is, which
    // ends the stream, and "done" fires for listeners such as the run page.
    resync(event) {
      const el = event.currentTarget;
      if (!el.dataset.synced) {
        el.dataset.synced = '1'; // the first connect follows a fresh render
        return;
      }
      fetch(el.dataset.syncUrl, { headers: { 'HX-Request': 'true' } })
        .then((res) => (res.ok ? res.text() : ''))
        .then((html) => {
          const tpl = document.createElement('template');
          tpl.innerHTML = html.trim();
          const fresh = tpl.content.firstElementChild;
          if (!fresh || !el.isConnected) return;
          if (fresh.hasAttribute('hx-sse:connect')) {
            el.replaceChildren(...fresh.childNodes);
            htmx.process(el);
            return;
          }
          const parent = el.parentElement;
          el.replaceWith(fresh);
          htmx.process(fresh);
          parent?.dispatchEvent(new CustomEvent('done', { bubbles: true }));
        })
        .catch(() => {});
    },
    // resyncRuns asks for recent runs after the feed's stream reconnects, so
    // runs started while the tab was hidden still appear.
    resyncRuns(event) {
      const el = event.currentTarget;
      if (!el.dataset.synced) {
        el.dataset.synced = '1';
        return;
      }
      fetch('/runs/recent')
        .then((res) => (res.ok ? res.json() : []))
        .then((ids) => {
          for (const id of [...ids].reverse()) this.addRun({ detail: { data: id } });
        })
        .catch(() => {});
    },
    // ask answers every hx-confirm with the in-page dialog instead of the
    // browser's. htmx waits until issueRequest or dropRequest is called.
    // The asking element names its action (data-confirm-ok), the way out
    // (data-confirm-no), and whether it destroys something
    // (data-confirm-tone="danger").
    // ask answers every hx-confirm with the in-page dialog instead of the
    // browser's. htmx waits until issueRequest or dropRequest is called.
    // The asking element names its action (data-confirm-ok), the way out
    // (data-confirm-no), and whether it destroys something
    // (data-confirm-tone="danger").
    ask(event) {
      const detail = event.detail;
      if (!detail?.ctx?.confirm || typeof detail.issueRequest !== 'function') return;
      event.preventDefault();
      const source = event.target.closest?.('[hx-confirm]') || event.target;
      this.confirm(detail.ctx.confirm, source.dataset?.confirmOk, source.dataset?.confirmNo,
        source.dataset?.confirmTone === 'danger').then((yes) => (yes ? detail.issueRequest() : detail.dropRequest()));
    },
    // confirm opens the dialog and resolves with the answer. A second ask
    // while one is open answers the first with no.
    confirm(text, ok, no, danger) {
      pending?.(false);
      return new Promise((resolve) => {
        pending = resolve;
        this.$refs.confirmText.textContent = text;
        this.$refs.confirmOk.textContent = ok || 'Continue';
        this.$refs.confirmNo.textContent = no || 'Cancel';
        this.$refs.confirmOk.classList.toggle('is-danger', Boolean(danger));
        this.$refs.confirm.showModal();
        (danger ? this.$refs.confirmNo : this.$refs.confirmOk).focus();
      });
    },
    answer(yes) {
      const p = pending;
      pending = null;
      this.$refs.confirm.close();
      p?.(yes);
    },
    // Esc closes the dialog without an answer, which counts as no.
    confirmClosed() {
      const p = pending;
      pending = null;
      p?.(false);
    },
    // clicked routes body clicks: previews, and the Settings dialog's
    // tabs, close button, and diagnostics.
    clicked(event) {
      const mark = event.target.closest?.('[data-error]');
      if (mark) {
        // A mark sits on cards and inside picker rows: its tap is not theirs.
        event.preventDefault();
        event.stopPropagation();
        const tip = this.$refs.errorTip;
        if (tip.classList.contains('is-open') && tip.dataset.for === (mark.dataset.error || '')) this.hideError();
        else this.showError(mark);
        return undefined;
      }
      this.hideError();
      this.openLightbox(event);
      const play = event.target.closest?.('[data-play-audio]');
      if (play?.dataset.playAudio) return this.playAudio(play);
      const nav = event.target.closest?.('[data-settings-nav]');
      if (nav) return this.settingsGo(nav.dataset.settingsNav);
      if (event.target.closest?.('[data-settings-close]')) return this.settingsClose();
      if (event.target.closest?.('[data-image-close]')) return this.closeImages();
      if (event.target.closest?.('[data-image-reset]')) {
        document.querySelector('[data-image-touched]').value = '';
        document.querySelector('[data-image-prompt]').value = '';
        return document.querySelector('.maker-body').dispatchEvent(new Event('change', { bubbles: true }));
      }
      if (event.target.closest?.('[data-copy-diagnostics]')) return this.copyDiagnostics();
      const toggle = event.target.closest?.('input[name="notify"]');
      if (toggle?.checked && 'Notification' in window && Notification.permission === 'default') {
        Notification.requestPermission();
      }
      return undefined;
    },
    // playAudio plays one audio reference at a time; a second click on the
    // same one stops it.
    playAudio(button) {
      const src = button.dataset.playAudio;
      const same = player && !player.paused && player.dataset.src === src;
      document.querySelectorAll('.is-playing').forEach((el) => el.classList.remove('is-playing'));
      if (player) player.pause();
      if (same) return undefined;
      player = new Audio(src);
      player.dataset.src = src;
      button.classList.add('is-playing');
      player.addEventListener('ended', () => button.classList.remove('is-playing'), { once: true });
      player.play().catch(() => {
        button.classList.remove('is-playing');
        this.toast('This audio cannot be played in the browser.');
      });
      return undefined;
    },
    settingsDirty() {
      return Boolean(document.querySelector('#settings-dialog [data-settings].is-dirty'));
    },
    settingsEdited(event) {
      if (event.target.name === 'purge_confirm') {
        const button = event.target.closest('.set-purge')?.querySelector('[data-purge-submit]');
        if (button) button.disabled = event.target.value !== 'PURGE';
        return;
      }
      const box = event.target.closest?.('#settings-dialog [data-settings]');
      if (box && event.target.closest('#settings-form') && event.target.name !== 'provider') box.classList.add('is-dirty');
    },
    async settingsGo(sec) {
      if (this.settingsDirty() && !(await this.confirm('Discard your unsaved changes?', 'Discard', 'Keep editing', true))) return;
      htmx.ajax('GET', `/settings?sec=${encodeURIComponent(sec)}`, { target: '#settings-dialog', swap: 'innerHTML' });
    },
    async settingsClose() {
      if (this.settingsDirty() && !(await this.confirm('Close without saving your changes?', 'Close', 'Keep editing', true))) return;
      this.$refs.settings.close();
      this.$refs.settings.replaceChildren();
      // A save can change engines, costs, and the server: show them.
      if (savedSettings) window.location.reload();
    },
    // swapped opens Settings when its content arrives, and notifies when a
    // run finishes in a background tab.
    swapped(event) {
      watchBentos();
      const target = event.target;
      // Settings opens whenever it has content and is closed; closing it
      // empties it, so later swaps elsewhere cannot reopen it.
      const dlg = this.$refs.settings;
      if (dlg.childElementCount > 0) {
        if (!dlg.open) dlg.showModal();
        if (dlg.querySelector('.settings-flash')?.textContent.startsWith('Saved')) savedSettings = true;
      }
      const images = this.$refs.images;
      if (images && images.childElementCount > 0 && !images.open) images.showModal();
      const editor = this.$refs.clipEditor;
      if (editor && editor.childElementCount > 0 && !editor.open) {
        editor.showModal();
        clipEditor(editor);
      }
      const card = target?.closest?.('.run-card');
      const status = card?.querySelector('.pill-status')?.textContent?.trim();
      if (card && (status === 'Done' || status === 'Failed') && document.hidden && document.body.dataset.notify === 'true'
        && 'Notification' in window && Notification.permission === 'granted' && !notified.has(card.id)) {
        notified.add(card.id);
        const title = card.querySelector('.card-title')?.textContent?.trim() || 'Utsushi';
        new Notification(status === 'Done' ? 'Run finished' : 'Run failed', { body: title, icon: '/static/icons/apple-touch-icon.png' });
      }
    },
    // Esc on Settings asks first when there are unsaved changes.
    settingsCancel(event) {
      event.preventDefault();
      this.settingsClose();
    },
    async copyDiagnostics() {
      try {
        const res = await fetch('/diagnostics');
        await navigator.clipboard.writeText(await res.text());
        this.toast('Diagnostics copied. Paste them into your report.');
      } catch {
        this.toast('Could not copy diagnostics. Open /diagnostics instead.');
      }
    },
    closeLightbox() {
      this.$refs.lightbox.close();
    },
    // Emptying the stage stops the video and releases its connection.
    lightboxClosed() {
      this.$refs.lightboxStage.replaceChildren();
    },
  };
});

// facts reads an engine's numbers from the picker, as viewEngine wrote them.
function facts(block) {
  try {
    return JSON.parse(block?.dataset.facts || '{}');
  } catch {
    return {};
  }
}

// splitsLong reports whether longer clips run in parts for this engine. A
// "30 s" clip measures 30.04 s and the like, so anything under longMax + 1
// counts, as on the server.
const splitsLong = (f) => Boolean(f.longMax && f.maxSeconds && f.maxSeconds < f.longMax + 1
  && f.maxSeconds > (f.overlap || 0) + (f.margin || 0));

// clipRange is an engine's clip length rule: "4-30 s", "≤ 30 s", or
// "≥ 4 s" when longer clips are trimmed rather than refused. An engine that
// runs longer clips in parts takes clips up to longMax.
const clipRange = (f) => {
  const most = splitsLong(f) ? f.longMax : f.maxSeconds;
  if (f.minSeconds && most) return `${f.minSeconds}-${most} s`;
  if (most) return `≤ ${most} s`;
  if (f.minSeconds) return `≥ ${f.minSeconds} s`;
  return '';
};

// partsFor mirrors the server's planParts: the segment lengths a clip longer
// than the engine takes runs in (one, trimmed, when it is barely over), or
// null when it runs whole. The server then moves the boundaries onto the
// clip's cuts, keeping the count.
const partsFor = (f, seconds) => {
  if (!splitsLong(f) || seconds <= f.maxSeconds || seconds >= f.longMax + 1) return null;
  const most = f.maxSeconds - (f.margin || 0);
  if (seconds - f.maxSeconds < (f.trimOver || 1)) return [most];
  const n = Math.ceil((seconds - f.overlap) / (most - f.overlap));
  return Array(n).fill((seconds + (n - 1) * f.overlap) / n);
};

// clipEditor runs the clip editor's timeline: the cuts as markers over the
// clip's scrub strip, a playhead that follows the video, click to move it,
// C or the button to cut there, click a cut to remove it. The cuts ride in
// hidden inputs that Save and Split send.
function clipEditor(dlg) {
  const ed = dlg.querySelector('[data-clip-editor]');
  if (!ed || ed.dataset.ready) return;
  ed.dataset.ready = '1';
  const video = ed.querySelector('[data-editor-video]');
  const line = ed.querySelector('[data-cut-timeline]');
  const head = ed.querySelector('[data-cut-playhead]');
  const marks = ed.querySelector('[data-cut-markers]');
  const length = Number(ed.dataset.seconds) || 1;
  const parse = (s) => {
    try {
      return JSON.parse(s || '[]');
    } catch {
      return [];
    }
  };
  let cuts = parse(ed.dataset.cuts);
  let trimStart = 0;
  let trimEnd = length;
  if (ed.dataset.strip) line.style.backgroundImage = `url("${ed.dataset.strip}")`;
  const drawTrim = () => {
    ed.querySelector('[data-trim-start-shade]').style.width = `${(trimStart / length) * 100}%`;
    ed.querySelector('[data-trim-end-shade]').style.width = `${((length - trimEnd) / length) * 100}%`;
    ed.querySelector('[data-trim-start-input]').value = trimStart.toFixed(3);
    ed.querySelector('[data-trim-end-input]').value = trimEnd.toFixed(3);
    const make = ed.querySelector('[data-trim-make]');
    const trimmed = trimStart > 0.05 || trimEnd < length - 0.05;
    const ok = trimmed && trimEnd - trimStart >= 1;
    make.disabled = !ok;
    make.textContent = ok ? `Trim to a new ${secs(trimEnd - trimStart)} clip` : trimmed ? 'Keep at least 1 s' : 'Trim to a new clip';
  };
  const setStart = () => {
    trimStart = Math.min(video.currentTime, trimEnd - 0.1);
    drawTrim();
  };
  const setEnd = () => {
    trimEnd = Math.max(video.currentTime, trimStart + 0.1);
    drawTrim();
  };
  const draw = () => {
    cuts = [...new Set(cuts.map((t) => Math.round(t * 1000) / 1000))]
      .filter((t) => t >= 0.5 && t <= length - 0.5).sort((a, b) => a - b);
    marks.replaceChildren(...cuts.map((t) => {
      const mark = Object.assign(document.createElement('button'), {
        type: 'button', className: 'cut-marker', title: `Cut at ${t.toFixed(2)} s · click to remove it`,
      });
      mark.dataset.t = String(t);
      mark.style.left = `${(t / length) * 100}%`;
      return mark;
    }));
    ed.querySelector('[data-cut-inputs]').replaceChildren(...cuts.map((t) => Object.assign(document.createElement('input'), {
      type: 'hidden', name: 'cuts', value: String(t),
    })));
    const bounds = [0, ...cuts, length];
    const pieces = bounds.slice(1).map((b, i) => b - bounds[i]);
    const shortest = Math.min(...pieces);
    ed.querySelector('[data-cut-summary]').textContent = `${plural(cuts.length, 'cut')} · ${plural(pieces.length, 'piece')}`;
    const split = ed.querySelector('[data-cut-split]');
    const ok = cuts.length >= 1 && cuts.length <= 2 && shortest >= 1;
    split.disabled = !ok;
    split.textContent = ok ? `Split into ${pieces.length} clips`
      : cuts.length > 2 ? 'Split makes 2 or 3 clips' : cuts.length ? 'Pieces must run 1 s or more' : 'Add a cut to split';
  };
  const at = (event) => {
    const r = line.getBoundingClientRect();
    return Math.min(Math.max((event.clientX - r.left) / r.width, 0), 1) * length;
  };
  line.addEventListener('click', (event) => {
    const mark = event.target.closest('.cut-marker');
    if (mark) {
      cuts = cuts.filter((t) => t !== Number(mark.dataset.t));
      draw();
      return;
    }
    video.currentTime = at(event);
  });
  video.addEventListener('timeupdate', () => { head.style.left = `${(video.currentTime / length) * 100}%`; });
  const add = () => {
    cuts.push(video.currentTime);
    draw();
  };
  ed.querySelector('[data-cut-add]').addEventListener('click', add);
  ed.querySelector('[data-cut-detected]').addEventListener('click', () => {
    cuts = parse(ed.dataset.detected);
    draw();
  });
  ed.querySelector('[data-cut-clear]').addEventListener('click', () => {
    cuts = [];
    draw();
  });
  ed.querySelector('[data-trim-start]').addEventListener('click', setStart);
  ed.querySelector('[data-trim-end]').addEventListener('click', setEnd);
  ed.addEventListener('keydown', (event) => {
    if (event.target.closest('input, textarea')) return;
    const keys = { c: add, i: setStart, o: setEnd };
    const run = keys[event.key.toLowerCase()];
    if (run) {
      event.preventDefault();
      run();
    }
  });
  ed.querySelector('[data-editor-close]').addEventListener('click', () => dlg.close());
  draw();
  drawTrim();
}

// kindOf is a reference checkbox's kind; image tiles predate the attribute.
const kindOf = (box) => box?.dataset.kind || 'image';

// tagsFor numbers each kind on its own, in pick order: images @1, videos
// @v1, audio @a1, as the server's refTags does.
const tagsFor = (ids, byId) => {
  const n = { image: 0, video: 0, audio: 0 };
  const prefix = { image: '', video: 'v', audio: 'a' };
  return new Map(ids.map((id) => {
    const kind = kindOf(byId.get(id));
    n[kind] += 1;
    return [id, `@${prefix[kind]}${n[kind]}`];
  }));
};

// Prompt tags. A mirror behind each prompt textarea draws the same text with
// tags the engine can read in lime and any other tag in amber, and typing @
// offers the tags in play.
const tagPattern = /@(?:clip|[va]?\d+)\b/g;
const escapeHTML = (s) => s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

// paintTags redraws a prompt's mirror from its text.
function paintTags(box, known) {
  const mirror = box.parentElement.querySelector('[data-prompt-mirror]');
  if (!mirror) return;
  const names = new Set(known.map((t) => t.tag));
  let html = '';
  let last = 0;
  for (const m of box.value.matchAll(tagPattern)) {
    html += `${escapeHTML(box.value.slice(last, m.index))}<mark class="${names.has(m[0]) ? 'is-tag' : 'is-stray'}">${m[0]}</mark>`;
    last = m.index + m[0].length;
  }
  // The zero-width space keeps a trailing newline's empty line.
  mirror.innerHTML = `${html}${escapeHTML(box.value.slice(last))}\u200b`;
  mirror.scrollTop = box.scrollTop;
}

// tagQuery is the partial @tag right before the caret, or null.
const tagQuery = (box) => {
  if (box.selectionStart !== box.selectionEnd) return null;
  const m = /(?:^|[\s(["'])(@\w*)$/.exec(box.value.slice(0, box.selectionStart));
  return m ? m[1] : null;
};

// caretRect is where the caret sits on screen, measured on the mirror.
function caretRect(box) {
  const mirror = box.parentElement.querySelector('[data-prompt-mirror]');
  mirror.textContent = box.value.slice(0, box.selectionStart);
  const mark = document.createElement('span');
  mark.textContent = '\u200b';
  mirror.append(mark);
  mirror.scrollTop = box.scrollTop;
  return mark.getBoundingClientRect();
}

// makeTagger runs the tag colors and @ menu for a page's prompt textareas
// (each inside a [data-tag-field] with a mirror and a menu). known returns
// the tags in play: { tag, label, thumb }.
function makeTagger(known) {
  let active = 0; // the highlighted entry of an open menu
  const hide = (box) => {
    const menu = box.parentElement.querySelector('[data-tag-menu]');
    if (menu) menu.hidden = true;
  };
  // show lists the tags that start with the partial @tag at the caret,
  // under the caret, and hides the menu when nothing fits.
  const show = (box) => {
    const menu = box.parentElement.querySelector('[data-tag-menu]');
    const q = tagQuery(box);
    const hits = q === null ? [] : known().filter((t) => t.tag.startsWith(q));
    if (!hits.length || (hits.length === 1 && hits[0].tag === q)) return hide(box);
    if (menu.hidden || active >= hits.length) active = 0;
    menu.replaceChildren(...hits.map((t, i) => {
      const option = Object.assign(document.createElement('button'), { type: 'button', className: 'tag-option', tabIndex: -1 });
      option.dataset.tag = t.tag;
      option.setAttribute('role', 'option');
      option.setAttribute('aria-selected', String(i === active));
      option.classList.toggle('is-active', i === active);
      const thumb = t.thumb ? Object.assign(document.createElement('img'), { src: t.thumb, alt: '' }) : document.createElement('span');
      thumb.className = 'tag-option-thumb';
      const tag = Object.assign(document.createElement('span'), { className: 'tag-option-tag', textContent: t.tag });
      const name = Object.assign(document.createElement('span'), { className: 'tag-option-name', textContent: t.label });
      option.append(thumb, tag, name);
      return option;
    }));
    const at = caretRect(box);
    paintTags(box, known());
    menu.hidden = false;
    menu.style.left = `${Math.max(8, Math.min(at.left - 8, window.innerWidth - menu.offsetWidth - 8))}px`;
    const below = at.bottom + 6;
    menu.style.top = `${below + menu.offsetHeight > window.innerHeight - 8 ? at.top - menu.offsetHeight - 6 : below}px`;
    return undefined;
  };
  // accept replaces the partial @tag at the caret with a whole one.
  const accept = (box, tag) => {
    const q = tagQuery(box) || '';
    const at = box.selectionStart;
    const after = box.value.slice(at);
    box.setRangeText(/^\s/.test(after) ? tag : `${tag} `, at - q.length, at, 'end');
    hide(box);
    box.dispatchEvent(new Event('input', { bubbles: true }));
  };
  // key drives an open menu: arrows move, Enter or Tab inserts, Escape
  // closes. It reports whether it used the key.
  const key = (event, box) => {
    const menu = box.parentElement.querySelector('[data-tag-menu]');
    if (!menu || menu.hidden) return false;
    const options = [...menu.querySelectorAll('.tag-option')];
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      active = (active + (event.key === 'ArrowDown' ? 1 : options.length - 1)) % options.length;
      options.forEach((o, i) => {
        o.classList.toggle('is-active', i === active);
        o.setAttribute('aria-selected', String(i === active));
      });
    } else if (event.key === 'Enter' || event.key === 'Tab') {
      accept(box, options[active].dataset.tag);
    } else if (event.key === 'Escape') {
      hide(box);
    } else {
      return false;
    }
    event.preventDefault();
    event.stopPropagation();
    return true;
  };
  return {
    show, hide, accept, key,
    // paintAll redraws every prompt's tags, and the menu of the one in focus.
    paintAll() {
      document.querySelectorAll('[data-tag-field] textarea').forEach((box) => {
        paintTags(box, known());
        if (document.activeElement === box) show(box);
      });
    },
    // listen wires the document-wide parts once: scroll does not bubble, so
    // the mirrors follow their textareas here, and a pick in the menu must
    // not take focus from the prompt.
    listen() {
      document.addEventListener('scroll', (event) => {
        if (!event.target.matches?.('[data-tag-field] textarea')) return;
        event.target.parentElement.querySelector('[data-prompt-mirror]').scrollTop = event.target.scrollTop;
        hide(event.target);
      }, true);
      document.addEventListener('mousedown', (event) => {
        const option = event.target.closest?.('.tag-option');
        if (!option) return;
        event.preventDefault();
        accept(option.closest('[data-tag-field]').querySelector('textarea'), option.dataset.tag);
      });
      document.addEventListener('focusout', (event) => {
        if (event.target.matches?.('[data-tag-field] textarea')) hide(event.target);
      });
    },
  };
}

const engineLabel = (root) => root.querySelector('#engine-block')?.dataset.engineName || 'This engine';
const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;
const secs = (s) => `${Number(s).toFixed(s % 1 ? 1 : 0)} s`;

// studio keeps the create panel's derived state in step with its inputs:
// reference order and @tags, the clip length check, and what Generate will
// do and cost. Every number comes from the server-rendered engine facts.
// The library flyouts (details.picker) behave the same on every page that
// has them: the studio's clip and reference pickers, and the Tools page's.
// An outside click closes any open one, except a click inside a dialog (the
// confirm before removing an image, the lightbox).
let pickersListening = false;
function listenPickers() {
  if (pickersListening) return;
  pickersListening = true;
  document.addEventListener('click', (event) => {
    if (event.target.closest?.('dialog')) return;
    document.querySelectorAll('.picker[open]').forEach((open) => {
      if (!open.contains(event.target)) open.open = false;
    });
  });
}

// pickerClick handles Done and places a flyout beside its row as it opens.
// It reports whether the click was one of those.
function pickerClick(event) {
  const done = event.target.closest?.('[data-picker-close]');
  if (done) {
    const picker = done.closest('.picker');
    picker.open = false;
    picker.querySelector('summary')?.focus();
    return true;
  }
  const summary = event.target.closest?.('.picker > summary');
  if (summary) {
    const pop = summary.parentElement.querySelector('.popover');
    const top = summary.getBoundingClientRect().top - 40;
    pop.style.top = `${Math.max(64, Math.min(top, window.innerHeight - pop.offsetHeight - 16))}px`;
    requestAnimationFrame(() => {
      const t = summary.getBoundingClientRect().top - 40;
      pop.style.top = `${Math.max(64, Math.min(t, window.innerHeight - pop.offsetHeight - 16))}px`;
    });
    return true;
  }
  return false;
}

// pickerEscape closes the open flyout on Esc.
function pickerEscape(event) {
  if (event.key !== 'Escape') return;
  const open = document.querySelector('.picker[open]');
  if (open) {
    open.open = false;
    open.querySelector('summary')?.focus();
  }
}

// A clip dropped anywhere on the dropzone or clip card is uploaded as if
// it had been browsed for.
function clipDragOver(event) {
  if (event.target.closest?.('[data-dropzone], [data-clip-card]')) event.preventDefault();
}

function clipDrop(event) {
  const zone = event.target.closest?.('[data-dropzone], [data-clip-card]');
  const file = event.dataTransfer?.files?.[0];
  if (!zone || !file) return;
  event.preventDefault();
  const input = document.getElementById('video-file');
  const list = new DataTransfer();
  list.items.add(file);
  input.files = list.files;
  input.dispatchEvent(new Event('change', { bubbles: true }));
}

// rememberRefs keeps a page's reference pick on the server, in pick order,
// so a reload opens with it, as it does with the clip. It waits a moment so
// a run of clicks sends once.
const refSaves = {};
function rememberRefs(mode, ids) {
  clearTimeout(refSaves[mode]);
  refSaves[mode] = setTimeout(() => {
    let token = '';
    try {
      token = JSON.parse(document.body.getAttribute('hx-headers:inherited') || '{}')['X-CSRF-Token'] || '';
    } catch {
      token = '';
    }
    const body = new URLSearchParams([['mode', mode], ...ids.map((id) => ['refs', id])]);
    fetch('/studio/refs', { method: 'POST', body, headers: { 'X-CSRF-Token': token } }).catch(() => {});
  }, 300);
}

HyperChi.registerComponent('studio', () => {
  let order = []; // reference ids in the order they were picked
  let style = ''; // the chosen style, kept across swatch swaps
  let before = ''; // the prompt before the language model changed it
  let known = []; // tags the engine reads: { tag, label, thumb }
  let saved = null; // the pick last remembered, joined
  const tagger = makeTagger(() => known);
  return {
    init() {
      // The page opens with the pick last made in this mode, in its order.
      const root = document.querySelector('.studio');
      try {
        order = JSON.parse(root?.dataset.picked || '[]');
      } catch {
        order = [];
      }
      for (const id of order) {
        const box = root.querySelector(`input[data-ref="${id}"]`);
        if (box) box.checked = true;
      }
      saved = order.join(',');
      queueMicrotask(() => this.update());
      tagger.listen();
      listenPickers();
    },
    // update runs on every change and swap inside the studio.
    update() {
      const root = document.querySelector('.studio');
      if (!root) return;
      const block = root.querySelector('#engine-block');
      const f = facts(block);
      const engineName = block?.dataset.engineName || 'This engine';
      // Kinds this engine takes; a picked file of another kind is let go.
      const allow = { image: true, video: (f.maxVideoRefs || 0) > 0, audio: (f.maxAudioRefs || 0) > 0 };
      const boxes = [...root.querySelectorAll('input[data-ref]')];
      for (const b of boxes) if (b.checked && !allow[kindOf(b)]) b.checked = false;
      const checked = new Set(boxes.filter((b) => b.checked).map((b) => b.dataset.ref));
      order = order.filter((id) => checked.has(id));
      for (const id of checked) if (!order.includes(id)) order.push(id);
      const byId = new Map(boxes.map((b) => [b.dataset.ref, b]));
      if (saved !== null && order.join(',') !== saved) {
        saved = order.join(',');
        const mode = root.querySelector('#run-form input[name="mode"]')?.value;
        if (mode) rememberRefs(mode, order);
      }
      const tags = tagsFor(order, byId);
      const count = { image: 0, video: 0, audio: 0 };
      const refSecs = { video: 0, audio: 0 };
      for (const id of order) {
        const b = byId.get(id);
        const kind = kindOf(b);
        count[kind] += 1;
        if (kind !== 'image') refSecs[kind] += Number(b.dataset.seconds || 0);
      }
      for (const b of boxes) {
        const tag = b.closest('.tile')?.querySelector('.tile-tag');
        if (tag) tag.textContent = tags.get(b.dataset.ref) || '';
      }
      const inputs = root.querySelector('[data-refs-inputs]');
      inputs.replaceChildren(...order.map((id) => Object.assign(document.createElement('input'), { type: 'hidden', name: 'refs', value: id })));

      const media = allow.video || allow.audio;
      root.querySelectorAll('[data-cast-count]').forEach((el) => {
        const images = f.maxRefs ? `${count.image} of ${f.maxRefs}` : `${count.image} picked`;
        el.textContent = media ? [`${images} images`.replace(' picked images', ' images'),
          allow.video && count.video ? plural(count.video, 'video') : '',
          allow.audio && count.audio ? `${count.audio} audio` : ''].filter(Boolean).join(' · ') : images;
      });
      root.querySelectorAll('[data-kind-count]').forEach((el) => { el.textContent = count[el.dataset.kindCount] || ''; });
      this.showTabs(root, allow);
      const ruleVideo = root.querySelector('[data-kind-rule="video"]');
      if (ruleVideo) ruleVideo.textContent = `${count.video} of ${f.maxVideoRefs || 0} · clip and videos ≤ ${f.refVideoTotal || 0} s`;
      const ruleAudio = root.querySelector('[data-kind-rule="audio"]');
      if (ruleAudio) ruleAudio.textContent = `${count.audio} of ${f.maxAudioRefs || 0} · ≤ ${f.refAudioTotal || 0} s in all`;
      const foot = root.querySelector('[data-pick-foot]');
      if (foot) foot.textContent = media ? 'Tags count each kind in pick order: images @1, videos @v1, audio @a1' : 'JPEG, PNG, or WebP · picked in order as @1, @2';
      root.querySelectorAll('[data-empty]').forEach((el) => {
        const grid = el.previousElementSibling;
        el.hidden = (grid?.querySelectorAll('.tile').length || 0) > 0;
      });
      this.showPicked(root, byId, tags, f);
      this.growPrompt();

      const tagsOn = block?.dataset.tags === 'true';
      const clipThumb = root.querySelector('#clip-block .clip-thumb video');
      known = tagsOn ? [
        ...order.map((id) => {
          const b = byId.get(id);
          const img = b.closest('.tile')?.querySelector('img');
          return { tag: tags.get(id), label: b.dataset.label || kindOf(b), thumb: img?.src || '' };
        }),
        { tag: '@clip', label: 'The source clip', thumb: clipThumb?.poster || '' },
      ] : [];
      const tagRow = root.querySelector('[data-prompt-tags]');
      if (tagRow) {
        tagRow.querySelector('[data-tag-list]').replaceChildren(...(tagsOn ? order : []).map((id) => Object.assign(document.createElement('button'), {
          type: 'button', className: 'chip chip-tag', textContent: tags.get(id),
        })));
        tagRow.querySelectorAll('[data-tag-list] button').forEach((b) => { b.dataset.insert = b.textContent; });
        tagRow.querySelector('.chip-clip').hidden = !tagsOn;
        tagRow.querySelector('[data-tag-hint]').textContent = tagsOn
          ? 'type @ to reference'
          : `${engineName} does not read @tags. Describe the references in words.`;
        tagRow.classList.toggle('is-off', !tagsOn);
      }
      tagger.paintAll();

      const clip = root.querySelector('#clip-block');
      const seconds = Number(clip?.dataset.seconds || 0);
      const card = root.querySelector('[data-clip-card]');
      const warning = root.querySelector('[data-clip-warning]');
      let clipProblem = '';
      let clipNote = '';
      const parts = partsFor(f, seconds);
      if (seconds > 0 && f.minSeconds && seconds < f.minSeconds) {
        clipProblem = `Too short. ${engineName} needs at least ${secs(f.minSeconds)}.`;
      } else if (parts && parts.length === 1) {
        clipNote = `${engineName} takes up to ${secs(f.maxSeconds)}, so it uses the first ${secs(parts[0])} of this clip.`;
      } else if (parts) {
        clipNote = `${engineName} takes up to ${secs(f.maxSeconds)}, so this clip runs in ${parts.length} parts, split at its cuts where they fit and joined into one.`;
      } else if (seconds > 0 && f.maxSeconds && seconds > f.maxSeconds) {
        clipProblem = `Too long. ${engineName} takes up to ${secs(splitsLong(f) ? f.longMax : f.maxSeconds)}.`;
      } else if (seconds > 0 && f.trimSeconds && seconds > f.trimSeconds) {
        clipNote = `${engineName} uses the first ${secs(f.trimSeconds)} of this clip.`;
      }
      if (warning) {
        warning.textContent = clipProblem || clipNote;
        warning.hidden = !(clipProblem || clipNote);
        warning.classList.toggle('is-note', !clipProblem);
      }
      card?.classList.toggle('is-warn', Boolean(clipProblem));
      const limits = root.querySelector('[data-clip-limits]');
      const range = clipRange(f);
      if (limits) limits.textContent = range ? `${range} · MP4 ≤ 200 MB` : 'MP4 ≤ 200 MB';
      const clipHead = root.querySelector('[data-clip-rule-head]');
      if (clipHead) clipHead.textContent = range || 'any length';

      // A swatch finishing re-renders its tile unchecked; keep the choice.
      const styleBoxes = [...root.querySelectorAll('input[name="style"]')];
      const checkedStyle = styleBoxes.find((b) => b.checked);
      if (checkedStyle) style = checkedStyle.value;
      else if (style) {
        const again = styleBoxes.find((b) => b.value === style);
        if (again) again.checked = true;
      }
      const needsStyle = styleBoxes.length > 0;
      const hasStyle = styleBoxes.some((b) => b.checked);

      const versions = Number(root.querySelector('input[name="versions"]:checked')?.value || 1);
      const resolution = root.querySelector('input[name="resolution"]:checked')?.value;
      let blocked = '';
      if (!seconds) blocked = 'Pick a source clip';
      else if (needsStyle && !hasStyle) blocked = 'Pick a style';
      else if (clipProblem) blocked = `Clip must be ${clipRange(f)}`;
      else if (count.image < (f.minRefs || 0)) blocked = `Pick ${plural(f.minRefs, 'reference image')}`;
      else if (f.maxRefs !== undefined && count.image > f.maxRefs) blocked = `Use at most ${plural(f.maxRefs, 'image')}`;
      else if (count.video > (f.maxVideoRefs || 0)) blocked = `Use at most ${plural(f.maxVideoRefs || 0, 'video')}`;
      else if (count.audio > (f.maxAudioRefs || 0)) blocked = `Use at most ${f.maxAudioRefs || 0} audio`;
      else if (f.refVideoTotal && count.video && (parts ? parts[0] : seconds) + refSecs.video > f.refVideoTotal) blocked = `Clip and videos over ${f.refVideoTotal} s`;
      else if (f.refAudioTotal && refSecs.audio > f.refAudioTotal) blocked = `Audio over ${f.refAudioTotal} s`;
      else if (f.promptRequired && !root.querySelector('#prompt').value.trim()) blocked = 'Describe the change';
      const note = root.querySelector('[data-prompt-note]');
      if (note) note.textContent = f.promptRequired ? 'required' : 'optional';

      const form = root.querySelector('#run-form');
      let spend = {};
      try {
        spend = JSON.parse(form.dataset.spend || '{}');
      } catch {
        spend = {};
      }
      // usd is the run's estimate, null when the provider publishes no price.
      let usd = null;
      let cost;
      const rate = f.rates ? f.rates[resolution] : undefined;
      if (!f.billed) {
        cost = 'free';
        usd = 0;
      } else if (rate !== undefined) {
        // Output runs as long as the clip; Wan 3.0 rounds to whole seconds.
        const out = (c) => Math.min(Math.max(Math.round(c), f.minOutput || 2), f.maxSeconds || c);
        // Some models bill the clip they are given as well as their output,
        // and a clip in parts bills each part.
        const input = (f.inputRates && f.inputRates[resolution]) || 0;
        usd = (parts || [seconds]).reduce((sum, c) => sum + rate * out(c) + input * c, 0) * versions;
        cost = `≈ $${usd.toFixed(2)}`;
      } else cost = `billed by ${f.via}`;
      const overCap = f.billed && spend.capOn && usd !== null && spend.spent + usd > spend.cap;
      if (!blocked && overCap && spend.capAction === 'block') blocked = `Over this month's $${spend.cap} cap`;

      const button = root.querySelector('[data-generate]');
      const meta = root.querySelector('[data-generate-meta]');
      button.disabled = Boolean(blocked);
      const showCost = !f.billed || spend.estimates !== false;
      meta.textContent = blocked || (showCost ? `${plural(versions, 'run')} · ${cost}` : plural(versions, 'run'));
      // Confirm a billed run over the threshold, one with no published
      // price, and one that passes the monthly cap.
      const threshold = typeof spend.confirmOver === 'number' ? spend.confirmOver : 2;
      let ask = '';
      if (f.billed && overCap) ask = `This run takes you past your $${spend.cap} monthly cap (${cost}). Start it?`;
      else if (f.billed && threshold >= 0 && (usd === null || usd > threshold)) ask = `Start ${plural(versions, 'billed run')} (${cost})?`;
      if (ask) {
        form.setAttribute('hx-confirm', ask);
        form.dataset.confirmOk = 'Generate';
      } else {
        form.removeAttribute('hx-confirm');
      }
    },
    // showPicked mirrors the picked references into the panel, in pick
    // order, and writes the engine's reference rule beside them.
    showPicked(root, byId, tags, f) {
      const strip = root.querySelector('[data-picked]');
      if (!strip) return;
      strip.replaceChildren(...order.map((id) => {
        const box = byId.get(id);
        const kind = kindOf(box);
        const tile = box?.closest('.tile');
        const item = document.createElement('div');
        item.className = `picked-item is-${kind}`;
        if (kind === 'audio') {
          item.innerHTML = tile?.querySelector('.tile-audio-face svg')?.outerHTML || '';
          item.title = `${box.dataset.label || 'Audio'} · ${Number(box.dataset.seconds || 0).toFixed(1)} s`;
          item.dataset.playAudio = tile?.querySelector('[data-play-audio]')?.dataset.playAudio || '';
        } else {
          const img = tile?.querySelector('img');
          const src = img?.getAttribute('src') || tile?.querySelector('video')?.getAttribute('src') || '';
          const el = img || kind === 'image'
            ? Object.assign(document.createElement('img'), { src, alt: '' })
            : Object.assign(document.createElement('video'), { src, muted: true, playsInline: true, preload: 'metadata' });
          item.append(el);
          const peek = tile?.querySelector('[data-peek]');
          if (peek) {
            item.dataset.peek = peek.dataset.peek;
            item.dataset.peekSrc = peek.dataset.peekSrc;
          }
        }
        const tag = Object.assign(document.createElement('span'), { className: 'tile-tag', textContent: tags.get(id) });
        const drop = Object.assign(document.createElement('button'), {
          type: 'button', className: 'picked-remove', textContent: '×',
        });
        drop.dataset.unpick = id;
        drop.setAttribute('aria-label', `Remove ${tags.get(id)}`);
        item.append(tag, drop);
        return item;
      }));
      strip.hidden = order.length === 0;
      const over = f.maxRefs !== undefined && order.length > f.maxRefs;
      strip.classList.toggle('is-over', over);
      const rule = f.refRule || '';
      const cta = root.querySelector('[data-ref-cta]');
      const takesAny = f.maxRefs > 0 || f.maxVideoRefs > 0 || f.maxAudioRefs > 0;
      if (cta) cta.textContent = !takesAny ? 'This engine takes no references' : (order.length ? 'Change references' : 'Choose references');
      const line = root.querySelector('[data-ref-rule]');
      if (line) line.textContent = f.refsFor ? `${rule} · ${f.refsFor}` : rule;
      const head = root.querySelector('[data-ref-rule-head]');
      if (head) head.textContent = rule.toLowerCase();
      const forLine = root.querySelector('[data-refs-for]');
      if (forLine) forLine.textContent = f.refsFor || '';
    },
    // showTabs shows the kind tabs when the engine takes video or audio,
    // falling back to images when the open tab's kind is not taken.
    showTabs(root, allow) {
      const row = root.querySelector('[data-ref-tabs]');
      if (!row) return;
      row.hidden = !(allow.video || allow.audio);
      row.querySelectorAll('[data-ref-tab]').forEach((t) => { t.hidden = !allow[t.dataset.refTab]; });
      let current = row.querySelector('[aria-selected="true"]')?.dataset.refTab || 'image';
      if (!allow[current]) current = 'image';
      this.showTab(root, current);
    },
    showTab(root, kind) {
      root.querySelectorAll('[data-ref-tab]').forEach((t) => t.setAttribute('aria-selected', String(t.dataset.refTab === kind)));
      root.querySelectorAll('[data-ref-panel]').forEach((p) => { p.hidden = p.dataset.refPanel !== kind; });
    },
    // growPrompt fits the prompt box to its text, up to a cap set in CSS.
    growPrompt() {
      const box = document.getElementById('prompt');
      if (!box) return;
      box.style.height = 'auto';
      box.style.height = `${Math.max(84, box.scrollHeight)}px`;
    },
    // openEditor shows the prompt in a large dialog; edits flow back as typed.
    openEditor() {
      const dlg = document.getElementById('prompt-dialog');
      const box = document.getElementById('prompt');
      const big = dlg.querySelector('[data-prompt-big]');
      big.value = box.value;
      big.placeholder = box.placeholder;
      const f = facts(document.querySelector('#engine-block'));
      const count = () => {
        dlg.querySelector('[data-prompt-count]').textContent = f.promptMax ? `${big.value.length} / ${f.promptMax}` : `${big.value.length} characters`;
        dlg.querySelector('[data-prompt-count]').classList.toggle('is-over', Boolean(f.promptMax && big.value.length > f.promptMax));
      };
      big.oninput = () => {
        box.value = big.value;
        box.dispatchEvent(new Event('input', { bubbles: true }));
        count();
      };
      const tags = document.querySelector('[data-prompt-tags]');
      const chips = [...document.querySelectorAll('[data-tag-list] button')].map((b) => b.textContent);
      dlg.querySelector('[data-prompt-dialog-tags]').textContent = tags && !tags.hidden && chips.length
        ? `References: ${chips.join(' ')}${document.querySelector('.chip-clip') ? ' · @clip' : ''}` : '';
      dlg.querySelector('#prompt-dialog-title').textContent = `Prompt · ${engineLabel(document)}`;
      count();
      dlg.showModal();
      big.focus();
      big.setSelectionRange(big.value.length, big.value.length);
    },
    // pickTag inserts a clicked @tag chip into the prompt at the caret. A
    // click on the engine row places its popover beside the panel, and the
    // language-model chips rewrite the prompt.
    pickTag(event) {
      if (event.target.matches?.('[data-tag-field] textarea')) return this.showMenu(event.target);
      // Reuse prompt on a card of this studio's mode fills the prompt in
      // place, keeping the clip, references, and engine; another mode's
      // card follows its link to that studio.
      const reuse = event.target.closest?.('[data-reuse-prompt]');
      if (reuse && reuse.dataset.reuseMode === document.querySelector('#run-form input[name="mode"]')?.value) {
        event.preventDefault();
        const box = document.getElementById('prompt');
        box.value = reuse.dataset.reusePrompt;
        const pin = document.querySelector('[data-pin]');
        if (pin) pin.value = reuse.dataset.reuseRun || '';
        box.dispatchEvent(new Event('input', { bubbles: true }));
        box.focus();
        box.scrollIntoView({ block: 'center' });
        document.body.dispatchEvent(new CustomEvent('toast', { detail: { message: 'Prompt reused. Nothing was started.' } }));
        return undefined;
      }
      const ai = event.target.closest?.('[data-llm]');
      if (ai) return this.askLLM(ai);
      if (event.target.closest?.('[data-prompt-expand]')) return this.openEditor();
      if (event.target.closest?.('[data-prompt-done]')) {
        document.getElementById('prompt-dialog').close();
        return document.getElementById('prompt').focus();
      }
      const tab = event.target.closest?.('[data-ref-tab]');
      if (tab) return this.showTab(document.querySelector('.studio'), tab.dataset.refTab);
      const unpick = event.target.closest?.('[data-unpick]');
      if (unpick) {
        const box = document.querySelector(`input[data-ref="${unpick.dataset.unpick}"]`);
        if (box) box.checked = false;
        return this.update();
      }
      if (event.target.closest?.('[data-llm-undo]')) return this.undoLLM();
      if (pickerClick(event)) return undefined;
      const chip = event.target.closest?.('[data-insert]');
      if (!chip) return;
      const box = document.getElementById('prompt');
      const at = box.selectionStart ?? box.value.length;
      const before = box.value.slice(0, at);
      const text = (before && !/\s$/.test(before) ? ' ' : '') + chip.dataset.insert + ' ';
      box.setRangeText(text, at, box.selectionEnd ?? at, 'end');
      box.focus();
      box.dispatchEvent(new Event('input', { bubbles: true }));
    },
    showMenu(box) { return tagger.show(box); },
    hideMenu(box) { tagger.hide(box); },
    acceptTag(box, tag) { tagger.accept(box, tag); },
    menuKey(event, box) { return tagger.key(event, box); },
    // askLLM sends the form to a language-model feature and puts the reply
    // in the prompt, keeping what was there for Undo.
    async askLLM(button) {
      const root = document.querySelector('.studio');
      const box = document.getElementById('prompt');
      const status = root.querySelector('[data-llm-status]');
      const body = new URLSearchParams();
      body.set('prompt', box.value);
      body.set('mode', root.querySelector('input[name="mode"]').value);
      body.set('engine', root.querySelector('#engine-block input[name="engine"]')?.value || '');
      body.set('source', root.querySelector('#clip-block input[name="source"]')?.value || '');
      for (const id of order) body.append('refs', id);
      body.set('style', root.querySelector('input[name="style"]:checked')?.value || '');
      let token = '';
      try {
        token = JSON.parse(document.body.getAttribute('hx-headers:inherited') || '{}')['X-CSRF-Token'] || '';
      } catch {
        token = '';
      }
      root.querySelectorAll('[data-llm]').forEach((b) => { b.disabled = true; });
      status.textContent = button.dataset.llm === 'describe' ? 'Looking at @1…' : 'Looking at the clip and references…';
      try {
        const res = await fetch(`/llm/${button.dataset.llm}`, {
          method: 'POST', body, headers: { 'X-CSRF-Token': token, 'Content-Type': 'application/x-www-form-urlencoded' },
        });
        const reply = await res.json();
        if (!res.ok || reply.error) throw new Error(reply.error || res.statusText);
        before = box.value;
        box.value = reply.prompt;
        root.querySelector('[data-llm-undo]').hidden = false;
        status.textContent = 'Review it, then Generate.';
        this.update();
      } catch (err) {
        status.textContent = '';
        document.body.dispatchEvent(new CustomEvent('toast', { detail: { message: err.message } }));
      } finally {
        root.querySelectorAll('[data-llm]').forEach((b) => { b.disabled = false; });
      }
      return undefined;
    },
    undoLLM() {
      const box = document.getElementById('prompt');
      box.value = before;
      document.querySelector('[data-llm-undo]').hidden = true;
      document.querySelector('[data-llm-status]').textContent = '';
      this.update();
    },
    keys(event) {
      const box = event.target.closest?.('[data-tag-field]')?.querySelector('textarea');
      if (box === event.target && this.menuKey(event, box)) return;
      // Caret moves change the partial tag; read it after the key lands.
      if (box === event.target && /^(ArrowLeft|ArrowRight|Home|End)$/.test(event.key)) {
        requestAnimationFrame(() => this.showMenu(box));
      }
      pickerEscape(event);
    },
    dragOver: clipDragOver,
    drop: clipDrop,
  };
});

// longvideo runs the Long video tab: the cast in pick order with its tags,
// the shot list (add, remove, move, each shot's join and length), and what
// Generate will make. Each shot's choices ride in hidden inputs, in shot
// order, because radio groups need a name of their own per shot.
let shotSerial = 0;
HyperChi.registerComponent('longvideo', () => {
  let order = []; // cast image ids in pick order
  let castSaved = null; // the cast last remembered, joined
  let known = []; // the cast's tags: { tag, label, thumb }
  const tagger = makeTagger(() => known);
  const root = () => document.querySelector('.long');
  // name gives a shot's radio groups names of their own.
  const name = (shot) => {
    if (!shot.dataset.serial) {
      shotSerial += 1;
      shot.dataset.serial = String(shotSerial);
      shot.querySelectorAll('[data-join] input').forEach((r) => { r.name = `join-${shotSerial}`; });
      shot.querySelectorAll('[data-length] input').forEach((r) => { r.name = `length-${shotSerial}`; });
      shot.querySelectorAll('[data-shot-end-pick] input').forEach((r) => { r.name = `end-${shotSerial}`; });
    }
  };
  return {
    init() {
      tagger.listen();
      // Reuse opens the tab with an earlier take's cast, in its order.
      const el = root();
      try {
        order = JSON.parse(el?.dataset.cast || '[]');
      } catch {
        order = [];
      }
      for (const id of order) {
        const box = el.querySelector(`#images input[data-ref="${id}"]`);
        if (box) box.checked = true;
      }
      castSaved = order.join(',');
      queueMicrotask(() => this.update());
    },
    update() {
      const el = root();
      if (!el) return;
      const boxes = [...el.querySelectorAll('#images input[data-ref]')];
      const checked = new Set(boxes.filter((b) => b.checked).map((b) => b.dataset.ref));
      order = order.filter((id) => checked.has(id));
      for (const id of checked) if (!order.includes(id)) order.push(id);
      if (castSaved !== null && order.join(',') !== castSaved) {
        castSaved = order.join(',');
        rememberRefs('long', order);
      }
      const byId = new Map(boxes.map((b) => [b.dataset.ref, b]));
      const tags = tagsFor(order, byId);
      for (const b of boxes) {
        const tag = b.closest('.tile')?.querySelector('.tile-tag');
        if (tag) tag.textContent = tags.get(b.dataset.ref) || '';
      }
      el.querySelector('[data-refs-inputs]').replaceChildren(...order.map((id) => Object.assign(document.createElement('input'), { type: 'hidden', name: 'refs', value: id })));
      known = order.map((id) => {
        const b = byId.get(id);
        return { tag: tags.get(id), label: b.dataset.label || 'image', thumb: b.closest('.tile')?.querySelector('img')?.src || '' };
      });
      el.querySelector('[data-cast-count]').textContent = `${order.length} of 9 · ${order.length ? [...tags.values()].join(' ') : 'none picked'}`;

      const start = el.querySelector('input[name="start"]:checked');
      const extending = Boolean(start?.value);
      el.querySelector('[data-start-note]').textContent = extending ? `extend ${secs(Number(start.dataset.seconds || 0))} of video` : 'a prompt';
      el.querySelector('[data-shape]').hidden = extending;
      el.querySelector('[data-soundtrack-mode]').hidden = !el.querySelector('input[name="soundtrack"]:checked')?.value;

      const shots = [...el.querySelectorAll('[data-shots] [data-shot]')];
      let total = extending ? Number(start.dataset.seconds || 0) : 0;
      let missing = 0;
      // Leading shots an earlier take made, unchanged and made the same way,
      // are kept rather than made again.
      const pick = (n) => el.querySelector(`input[name="${n}"]:checked`)?.value || '';
      const setup = `${start?.value || ''}|${JSON.stringify(order)}|${pick('shape')}|${pick('size')}|${pick('quality')}|${pick('cast_view')}|${pick('ref_size')}`;
      const sameSetup = Boolean(el.dataset.orig) && setup === el.dataset.orig;
      let keep = 0;
      let prevKept = null; // the earlier index the shot before kept, or null
      shots.forEach((shot, i) => {
        name(shot);
        shot.querySelector('[data-shot-num]').textContent = `Shot ${i + 1}`;
        const first = i === 0 && !extending;
        const cont = shot.querySelector('[data-join] input[value="continue"]');
        cont.disabled = first;
        if (first && cont.checked) shot.querySelector('[data-join] input[value="cut"]').checked = true;
        const join = shot.querySelector('[data-join] input:checked')?.value || 'cut';
        const length = Number(shot.querySelector('[data-length] input:checked')?.value || 10);
        shot.querySelector('input[name="shot_join"]').value = join;
        shot.querySelector('input[name="shot_seconds"]').value = String(length);
        const end = shot.querySelector('[data-shot-end-pick] input:checked')?.value || '';
        shot.querySelector('input[name="shot_end"]').value = end;
        shot.querySelector('[data-shot-end-label]').textContent = end
          ? (shot.querySelector('[data-shot-end-pick] input:checked').closest('label').title || 'an image') : 'anything';
        shot.querySelector('[data-shot-up]').disabled = i === 0;
        shot.querySelector('[data-shot-down]').disabled = i === shots.length - 1;
        shot.querySelector('[data-shot-remove]').hidden = shots.length === 1;
        shot.classList.toggle('is-continuing', join === 'continue');
        const prompt = shot.querySelector('textarea').value.trim();
        const unchanged = shot.dataset.done !== undefined && shot.dataset.remake === undefined
          && prompt === (shot.dataset.origPrompt || '').trim() && String(length) === shot.dataset.origSeconds
          && join === shot.dataset.origJoin && end === (shot.dataset.origEnd || '');
        // A kept shot starts a new shot, or follows the shot that came just
        // before it in the earlier take, also kept.
        const index = Number(shot.dataset.origIndex);
        const keeping = sameSetup && unchanged
          && (join === 'cut' || (i === 0 && index === 0) || (prevKept !== null && prevKept === index - 1));
        prevKept = keeping ? index : null;
        shot.querySelector('[data-shot-keep]').value = keeping ? String(index) : '';
        if (keeping) keep += 1;
        shot.classList.toggle('is-kept', keeping);
        shot.querySelector('[data-kept-note]').hidden = !keeping;
        shot.querySelector('[data-shot-remake]').hidden = !keeping;
        total += length;
        if (!prompt) missing += 1;
      });
      el.querySelector('[data-shots-note]').textContent = `${plural(shots.length, 'shot')} · ${secs(total)} in all`;

      const billed = el.dataset.billed === 'true';
      let blocked = '';
      if (el.dataset.offline) blocked = 'MiniMax H3 is not ready';
      else if (!shots.length) blocked = 'Add a shot';
      else if (missing) blocked = missing === 1 ? 'Say what happens in each shot' : `${missing} shots need a prompt`;
      else if (order.length > 9) blocked = 'The cast takes at most 9 images';
      else if (keep && keep === shots.length) blocked = 'Nothing changed: edit a shot or Remake one';
      const button = el.querySelector('[data-generate]');
      button.disabled = Boolean(blocked);
      const cost = billed ? `billed by ${el.dataset.via}` : 'free · your GPU';
      const making = keep ? ` · ${shots.length - keep} to make` : '';
      el.querySelector('[data-generate-meta]').textContent = blocked || `${plural(shots.length, 'shot')}${making} · ${secs(total)} · ${cost}`;
      const form = el.querySelector('#long-form');
      if (billed) {
        form.setAttribute('hx-confirm', `Start a ${secs(total)} long video on ${el.dataset.via}? It bills by GPU time and publishes no price.`);
        form.dataset.confirmOk = 'Generate';
      } else {
        form.removeAttribute('hx-confirm');
      }
      tagger.paintAll();
    },
    clicks(event) {
      if (event.target.matches?.('[data-tag-field] textarea')) return tagger.show(event.target);
      const el = root();
      const shot = event.target.closest?.('[data-shot]');
      if (event.target.closest?.('[data-shot-add]')) {
        const fresh = el.querySelector('[data-shot-template]').content.firstElementChild.cloneNode(true);
        el.querySelector('[data-shots]').append(fresh);
        this.update();
        return fresh.querySelector('textarea').focus();
      }
      if (shot && event.target.closest?.('[data-shot-end-toggle]')) {
        const pick = shot.querySelector('[data-shot-end-pick]');
        pick.hidden = !pick.hidden;
        return undefined;
      }
      const ai = event.target.closest?.('[data-shot-llm]');
      if (shot && ai) return this.askShot(shot, ai.dataset.shotLlm);
      if (shot && event.target.closest?.('[data-shot-undo]')) {
        const box = shot.querySelector('textarea');
        box.value = shot.dataset.undo || '';
        delete shot.dataset.undo;
        event.target.closest('[data-shot-undo]').hidden = true;
        shot.querySelector('[data-shot-status]').textContent = '';
        return this.update();
      }
      if (shot && event.target.closest?.('[data-shot-remake]')) {
        shot.dataset.remake = '';
        return this.update();
      }
      if (shot && event.target.closest?.('[data-shot-remove]')) {
        shot.remove();
        return this.update();
      }
      if (shot && event.target.closest?.('[data-shot-up]') && shot.previousElementSibling) {
        shot.parentElement.insertBefore(shot, shot.previousElementSibling);
        return this.update();
      }
      if (shot && event.target.closest?.('[data-shot-down]') && shot.nextElementSibling) {
        shot.parentElement.insertBefore(shot.nextElementSibling, shot);
        return this.update();
      }
      return undefined;
    },
    keys(event) {
      const box = event.target.closest?.('[data-tag-field]')?.querySelector('textarea');
      if (box === event.target && tagger.key(event, box)) return;
      if (box === event.target && /^(ArrowLeft|ArrowRight|Home|End)$/.test(event.key)) {
        requestAnimationFrame(() => tagger.show(box));
      }
    },
    // askShot has the language model draft (suggest) or rewrite (improve)
    // one shot, knowing the cast, the shot before, and how this one joins
    // it; the first shot of an extension also sees the video it continues.
    async askShot(shot, feature) {
      const el = root();
      const shots = [...el.querySelectorAll('[data-shots] [data-shot]')];
      const i = shots.indexOf(shot);
      const box = shot.querySelector('textarea');
      const status = shot.querySelector('[data-shot-status]');
      if (feature === 'improve' && !box.value.trim()) {
        status.textContent = 'Write a rough line first.';
        return;
      }
      const join = shot.querySelector('input[name="shot_join"]').value;
      const start = el.querySelector('input[name="start"]:checked')?.value || '';
      const body = new URLSearchParams();
      body.set('prompt', box.value);
      body.set('mode', 'long');
      body.set('engine', 'minimax-h3-long');
      body.set('source', i === 0 && join === 'continue' ? start : '');
      for (const id of order) body.append('refs', id);
      body.set('before', i > 0 ? shots[i - 1].querySelector('textarea').value : '');
      body.set('join', join);
      let token = '';
      try {
        token = JSON.parse(document.body.getAttribute('hx-headers:inherited') || '{}')['X-CSRF-Token'] || '';
      } catch {
        token = '';
      }
      const chips = shot.querySelectorAll('[data-shot-llm]');
      chips.forEach((b) => { b.disabled = true; });
      status.textContent = feature === 'suggest' ? 'Drafting from the cast…' : 'Rewriting…';
      try {
        const res = await fetch(`/llm/${feature}`, {
          method: 'POST', body, headers: { 'X-CSRF-Token': token, 'Content-Type': 'application/x-www-form-urlencoded' },
        });
        const reply = await res.json();
        if (!res.ok || reply.error) throw new Error(reply.error || res.statusText);
        shot.dataset.undo = box.value;
        box.value = reply.prompt;
        shot.querySelector('[data-shot-undo]').hidden = false;
        status.textContent = 'Review it, then Generate.';
        this.update();
      } catch (err) {
        status.textContent = '';
        document.body.dispatchEvent(new CustomEvent('toast', { detail: { message: err.message } }));
      } finally {
        chips.forEach((b) => { b.disabled = false; });
      }
    },
  };
});

// feed filters the run cards by state and counts each state. Cards carry
// their state on an inner element, so live swaps keep the counts honest.
HyperChi.registerComponent('feed', () => ({
  init() {
    queueMicrotask(() => this.recount());
    const cards = document.getElementById('runs');
    if (cards) new MutationObserver(() => this.recount()).observe(cards, { childList: true, subtree: true });
  },
  show(event) {
    const button = event.currentTarget;
    const feed = button.closest('.feed');
    feed.querySelectorAll('[data-filter]').forEach((b) => b.setAttribute('aria-selected', String(b === button)));
    feed.querySelector('#runs').dataset.filter = button.dataset.filter;
  },
  recount() {
    const cards = document.getElementById('runs');
    if (!cards) return;
    const groups = [...cards.querySelectorAll('.card[data-group]')].map((c) => c.dataset.group);
    const counts = { all: groups.length, running: 0, done: 0, failed: 0 };
    for (const g of groups) counts[g] = (counts[g] || 0) + 1;
    document.querySelectorAll('[data-count]').forEach((el) => { el.textContent = String(counts[el.dataset.count] ?? 0); });
  },
}));

// tools runs the Tools page's form for whichever tool it shows. The source
// is the studio's clip block, or one image from the same flyout the studio
// picks references from. The button says what it will make, or why it
// cannot.
HyperChi.registerComponent('tools', () => {
  let shownAt = -1; // the time the frame preview last sought to
  const outWord = { frame: 'image', conform: 'clip', audio: 'audio file', interp: 'clip' };
  return {
    init() {
      listenPickers();
      queueMicrotask(() => this.update());
    },
    clicks(event) {
      if (pickerClick(event)) return;
      if (event.target.closest?.('[data-unpick]')) {
        document.querySelectorAll('.tools #images input[data-ref]').forEach((b) => { b.checked = false; });
        this.update();
        return;
      }
      const step = event.target.closest?.('[data-frame-step]');
      if (step) this.stepFrame(step.dataset.frameStep);
    },
    keys: pickerEscape,
    dragOver: clipDragOver,
    drop: clipDrop,
    // changed keeps one image picked: the newest check wins.
    changed(event) {
      const box = event.target;
      if (box.matches?.('#images input[data-ref]') && box.checked) {
        document.querySelectorAll('.tools #images input[data-ref]').forEach((b) => { if (b !== box) b.checked = false; });
      }
      this.update();
    },
    update() {
      const el = document.querySelector('.tools');
      if (!el) return;
      const tool = el.dataset.tool;
      const kind = el.querySelector('input[name="source_kind"]:checked, input[type="hidden"][name="source_kind"]')?.value || 'video';
      el.querySelectorAll('[data-source-pane]').forEach((pane) => { pane.hidden = pane.dataset.sourcePane !== kind; });
      const head = el.querySelector('[data-clip-rule-head]');
      if (head) head.textContent = 'any length';
      const image = kind === 'image';
      const picked = el.querySelector('#images input[data-ref]:checked');
      el.querySelector('[data-image-input]').value = image && picked ? picked.dataset.ref : '';
      el.querySelector('[data-image-cta]').textContent = picked ? 'Change image' : 'Choose an image';
      this.showPicked(el, picked);
      const ready = image ? Boolean(picked) : Boolean(el.querySelector('#clip-block input[name="source"]'));

      let count = 1;
      let reason = '';
      if (tool === 'depth') {
        const models = [...el.querySelectorAll('input[name="model"]')];
        models.forEach((m) => { m.disabled = image && m.value === 'video'; });
        if (image) models.find((m) => m.value === 'frame').checked = true;
        const model = models.find((m) => m.checked);
        el.querySelector('[data-model-note]').textContent = image
          ? 'an image takes the sharp model'
          : (model?.dataset.first || '');
        count = el.querySelectorAll('input[name="look"]:checked').length;
        if (!count) reason = 'Pick a map';
      }
      if (tool === 'matte') {
        count = el.querySelectorAll('input[name="look"]:checked').length;
        if (!count) reason = 'Pick the matte or the cutout';
      }
      if (tool === 'upscale') {
        const note = el.querySelector('[data-model-note]');
        const factor = Number(el.querySelector('input[name="factor"]:checked')?.value || 2);
        const card = el.querySelector('#clip-block .clip-info .meta')?.textContent || '';
        const size = image ? '' : card.match(/(\d+)×(\d+)/);
        note.textContent = size ? `${Number(size[1]) * factor}×${Number(size[2]) * factor}` : '';
      }
      if (tool === 'frame') this.showFrame(el);
      if (tool === 'conform' && [...el.querySelectorAll('[name="rate"], [name="size"], [name="shape"]')]
        .filter((r) => r.checked).every((r) => r.value === 'keep')) {
        reason = 'Change the rate, size, or shape';
      }
      if (!ready) reason = image ? 'Pick an image' : 'Pick a clip';

      const word = outWord[tool] || (image ? 'image' : 'clip');
      const button = el.querySelector('[data-generate]');
      if (button.dataset.offline === undefined) button.dataset.offline = button.disabled ? '1' : '';
      button.disabled = Boolean(reason) || button.dataset.offline === '1';
      el.querySelector('[data-generate-meta]').textContent = reason
        || `${count} ${word}${count === 1 ? '' : 's'} · Free · this machine`;
    },
    // showFrame keeps Grab a frame's preview on the current clip and at the
    // slider's time.
    showFrame(el) {
      const video = el.querySelector('[data-frame-preview]');
      const range = el.querySelector('[data-frame-range]');
      const block = el.querySelector('#clip-block');
      const src = (block?.querySelector('.clip-thumb video')?.getAttribute('src') || '').replace(/#.*$/, '');
      const seconds = Number(block?.dataset.seconds || 0);
      range.max = String(Math.max(0, seconds - 0.05).toFixed(2));
      if (Number(range.value) > Number(range.max)) range.value = range.max;
      if (video.getAttribute('src') !== src) {
        shownAt = -1;
        if (src) video.setAttribute('src', src);
        else video.removeAttribute('src');
      }
      const at = Number(range.value);
      el.querySelector('[data-frame-at]').textContent = `${at.toFixed(2)} s`;
      if (src && at !== shownAt) {
        shownAt = at;
        video.currentTime = at;
      }
    },
    stepFrame(step) {
      const range = document.querySelector('.tools [data-frame-range]');
      if (!range) return;
      const max = Number(range.max);
      if (step === 'first') range.value = '0';
      else if (step === 'last') range.value = String(max);
      else range.value = String(Math.min(max, Math.max(0, Number(range.value) + Number(step))).toFixed(2));
      this.update();
    },
    // showPicked shows the picked image in the panel, as the studio shows
    // its references.
    showPicked(el, picked) {
      const strip = el.querySelector('[data-picked]');
      const img = picked?.closest('.tile')?.querySelector('img');
      strip.hidden = !img;
      if (!img) {
        strip.replaceChildren();
        return;
      }
      const item = Object.assign(document.createElement('div'), { className: 'picked-item is-image' });
      item.dataset.peek = 'image';
      item.dataset.peekSrc = img.getAttribute('src');
      item.append(Object.assign(document.createElement('img'), { src: img.getAttribute('src'), alt: '' }));
      const drop = Object.assign(document.createElement('button'), {
        type: 'button', className: 'picked-remove', textContent: '×', title: 'Remove',
      });
      drop.dataset.unpick = picked.dataset.ref;
      drop.setAttribute('aria-label', `Remove ${picked.dataset.label || 'the image'}`);
      item.append(drop);
      strip.replaceChildren(item);
    },
  };
});

const clock = (t) => {
  if (!Number.isFinite(t)) return '0:00.0';
  const m = Math.floor(t / 60);
  return `${m}:${(t - m * 60).toFixed(1).padStart(4, '0')}`;
};

// viewer drives the result viewer: the split handle, the three views, and
// one transport for both videos. The result leads; the source follows.
HyperChi.registerComponent('viewer', () => ({
  pos: 50,
  mode: 'split',
  playing: false,
  dragging: false,
  seek: 0,
  clock: '0:00.0 / 0:00.0',
  start(event) {
    if (this.mode !== 'split') return;
    this.dragging = true;
    event.currentTarget.setPointerCapture(event.pointerId);
    this.move(event);
  },
  move(event) {
    if (!this.dragging) return;
    const box = event.currentTarget.getBoundingClientRect();
    const x = Math.min(Math.max(event.clientX - box.left, 0), box.width);
    this.pos = Math.round((x / box.width) * 1000) / 10;
  },
  stop(event) {
    this.dragging = false;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
  },
  setMode() {
    this.$refs.stage.dataset.mode = this.mode;
  },
  toggle() {
    const { after, before } = this.$refs;
    if (after.paused) {
      before.currentTime = Math.min(after.currentTime, before.duration || 0);
      Promise.all([after.play(), before.play()]).then(
        () => { this.playing = true; },
        () => { this.playing = false; },
      );
    } else {
      after.pause();
      before.pause();
      this.playing = false;
    }
  },
  scrub() {
    const { after, before } = this.$refs;
    if (!after.duration) return;
    after.currentTime = (this.seek / 1000) * after.duration;
    before.currentTime = Math.min(after.currentTime, before.duration || 0);
  },
  sync() {
    const { after, before } = this.$refs;
    if (after.duration) {
      this.seek = Math.round((after.currentTime / after.duration) * 1000);
      this.clock = `${clock(after.currentTime)} / ${clock(after.duration)}`;
    }
    if (before.duration && Math.abs(before.currentTime - after.currentTime) > 0.08) {
      before.currentTime = Math.min(after.currentTime, before.duration);
    }
    if (after.paused !== before.paused) {
      if (after.paused) before.pause(); else before.play().catch(() => { this.playing = false; });
    }
  },
}));
