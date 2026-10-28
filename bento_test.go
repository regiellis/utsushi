package main

import (
	"os/exec"
	"strings"
	"testing"
)

// bentoHarness pulls the packer's functions out of app.js and lays out
// seeded random lists of tile shapes, reporting any layout that leaves a
// hole, overlaps, or runs outside the grid, and the library case.
const bentoHarness = `
const src = require('fs').readFileSync(0, 'utf8');
const pick = (name) => {
  const a = src.indexOf('function ' + name + '(');
  if (a < 0) throw new Error('no ' + name);
  let d = 0, i = src.indexOf('{', a);
  for (; i < src.length; i++) { if (src[i] === '{') d++; else if (src[i] === '}' && --d === 0) break; }
  return src.slice(a, i + 1);
};
const bentoLayout = new Function(pick('bentoBands') + pick('bentoFlush') + pick('bentoLayout') + 'return bentoLayout;')();
let seed = 7;
const rand = () => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; };
const kinds = ['', 'wide', 'tall'];
let bad = 0;
for (let t = 0; t < 1500; t++) {
  const n = 1 + Math.floor(rand() * 40), cols = 1 + Math.floor(rand() * 9), mix = rand();
  const shapes = Array.from({ length: n }, () => (rand() < mix ? 'tall' : kinds[Math.floor(rand() * 3)]));
  const at = bentoLayout(shapes, cols);
  const cells = new Set(); let rows = 0, err = '';
  shapes.forEach((_, i) => {
    const p = at[i];
    if (!p) { err = 'tile ' + i + ' unplaced'; return; }
    if (p.x < 0 || p.x + p.w > cols) err = 'outside';
    for (let y = p.y; y < p.y + p.h; y++) for (let x = p.x; x < p.x + p.w; x++) {
      const k = y + ',' + x; if (cells.has(k)) err = 'overlap'; cells.add(k);
    }
    rows = Math.max(rows, p.y + p.h);
  });
  if (!err && cells.size !== rows * cols) err = (rows * cols - cells.size) + ' holes';
  if (err) { bad++; if (bad < 4) console.log('BAD', err, cols, JSON.stringify(shapes)); }
}
console.log('bad ' + bad);
console.log('library ' + bentoLayout(Array(9).fill('tall'), 7).map((p) => p.w + 'x' + p.h).join(' '));
`

func TestBentoLeavesNoHoleAndNoOverlap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	js, err := staticFiles.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", bentoHarness)
	cmd.Stdin = strings.NewReader(string(js))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "bad 0\n") {
		t.Fatalf("layouts with holes, overlaps, or tiles outside the grid:\n%s", out)
	}
	// Nine portrait runs in seven columns, the Library page on 2026-10-18:
	// flush by promoting features, none stretched past two columns.
	if !strings.Contains(string(out), "library 2x2 1x2 2x2 1x2 1x2 2x2 2x2 1x2 2x2") {
		t.Fatalf("the library case packed differently:\n%s", out)
	}
}
