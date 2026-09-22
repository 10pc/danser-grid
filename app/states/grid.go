package states

// Grid span renderer (danser-grid MVP): static grid spans straight from
// replay files. One process renders a whole batch: tiles init once near t=0
// and the grid clock runs continuously across spans — no per-span seeks.
//
// Threading: RunGrid runs on the main thread (called from app.go's init
// closure). Ticks, draws and ffmpeg frame calls happen inline — no CallMain.
// Clocks are synthetic 1ms ticks; audio is virtual (video-only spans,
// downstream mixes from map mp3s). Cursors stay off in M1 (per-tile trail
// FBOs land in M2); bloom/blur off via the grid profile.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-gl/gl/v3.3-core/gl"
	"github.com/wieku/danser-go/app/beatmap"
	difficulty2 "github.com/wieku/danser-go/app/beatmap/difficulty"
	"github.com/wieku/danser-go/app/ffmpeg"
	"github.com/wieku/danser-go/app/settings"
	"github.com/wieku/danser-go/framework/graphics/buffer"
	"github.com/wieku/danser-go/framework/graphics/viewport"
	"github.com/wieku/danser-go/framework/goroutines"
	"github.com/wieku/rplpa"
)

// GridTileSpec is one tile's placement (absolute canvas pixels, top-down)
// + source. Static spans use X/Y/W/H. Morph spans lerp Ax/Ay/Aw/Ah into
// Bx/By/Bw/Bh over the span window; Dying tiles shrink toward their own
// center (Python precomputes the to-center endpoint) while playing out.
type GridTileSpec struct {
	Replay string `json:"replay"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Ax     int    `json:"ax,omitempty"`
	Ay     int    `json:"ay,omitempty"`
	Aw     int    `json:"aw,omitempty"`
	Ah     int    `json:"ah,omitempty"`
	Bx     int    `json:"bx,omitempty"`
	By     int    `json:"by,omitempty"`
	Bw     int    `json:"bw,omitempty"`
	Bh     int    `json:"bh,omitempty"`
	Dying  bool   `json:"dying,omitempty"`
}

// GridSpanSpec is one span: full-layout video for [start, end) in grid
// (wall-clock) seconds. Kind is "static" (tiles hold their rects) or
// "morph" (tiles lerp; "" means static for backward compatibility).
type GridSpanSpec struct {
	Name  string         `json:"name"`
	Start float64        `json:"start"`
	End   float64        `json:"end"`
	Kind  string         `json:"kind,omitempty"`
	Tiles []GridTileSpec `json:"tiles"`
}

// GridSpec is one batch: shared canvas, spans in time order.
type GridSpec struct {
	Width  int            `json:"width"`
	Height int            `json:"height"`
	FPS    int            `json:"fps"`
	OutDir string         `json:"outDir"`
	Spans  []GridSpanSpec `json:"spans"`
	// Overlay strings, computed outside danser-grid (e.g. by the
	// completionist pipeline). Absent/null disables that element.
	Header *GridHeaderSpec `json:"header,omitempty"`
	Outro  *GridOutroSpec  `json:"outro,omitempty"`
	Player *GridPlayerSpec `json:"player,omitempty"`
}

// GridHeaderSpec is one preformatted header line ("date | N maps").
type GridHeaderSpec struct {
	Line string `json:"line"`
}

// GridOutroSpec carries the two outro stat lines.
type GridOutroSpec struct {
	Line1 string `json:"line1"`
	Line2 string `json:"line2"`
}

// GridPlayerSpec identifies the single player for the card module:
// global rank as "#123", country as "ID", avatar and pre-blurred banner
// plate as local PNG paths (downloaded outside; danser-grid only reads).
type GridPlayerSpec struct {
	Username string `json:"username"`
	Rank     string `json:"rank"`
	Country  string `json:"country"`
	Avatar   string `json:"avatar"`
	Banner   string `json:"banner,omitempty"`
}

// GridTile is a live tile: its player plus source identity.
type GridTile struct {
	player   *Player
	replay   string
	label    string
	username string
	rect     [4]int
	done     bool
	failed   bool
}

// tickTile advances one tile, converting an upstream panic (bad slider
// data, corrupt replay edge, ...) into a dropped tile instead of a dead
// batch. Matches the build-time per-tile skip philosophy: one bad apple
// never kills 70+ good tiles. GL-safe: panics here come from update
// logic, never mid-draw (draws happen separately in drawGridFrame).
func tickTile(t *GridTile, updateDelta float64) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("grid tile FAIL %s: %v (dropping tile, batch continues)",
				t.replay, r)
			t.done = true
			t.failed = true
		}
	}()
	if t.player.Update(updateDelta) {
		t.done = true
	}
}

func loadGridSpec(path string) (*GridSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("grid spec: %w", err)
	}
	var spec GridSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("grid spec: %w", err)
	}
	if spec.Width <= 0 || spec.Height <= 0 || spec.FPS <= 0 || len(spec.Spans) == 0 {
		return nil, fmt.Errorf("grid spec: bad canvas/fps/spans")
	}
	if spec.OutDir == "" {
		return nil, fmt.Errorf("grid spec: missing outDir")
	}
	for i, s := range spec.Spans {
		if s.End <= s.Start || s.Name == "" || len(s.Tiles) == 0 {
			return nil, fmt.Errorf("grid spec: span %d bad window/name/tiles", i)
		}
		if s.Kind != "" && s.Kind != "static" && s.Kind != "morph" && s.Kind != "outro" {
			return nil, fmt.Errorf("grid spec: span %d bad kind %q", i, s.Kind)
		}
		if s.Kind == "outro" {
			continue
		}
		for j, t := range s.Tiles {
			if t.Replay == "" {
				return nil, fmt.Errorf("grid spec: span %d tile %d missing replay", i, j)
			}
			if s.Kind == "morph" {
				if t.Aw <= 0 || t.Ah <= 0 || t.Bw <= 0 || t.Bh <= 0 {
					return nil, fmt.Errorf("grid spec: span %d tile %d bad morph rect", i, j)
				}
			} else if t.W <= 0 || t.H <= 0 {
				return nil, fmt.Errorf("grid spec: span %d tile %d bad rect", i, j)
			}
		}
	}
	return &spec, nil
}

// buildGridTiles constructs one Player per DISTINCT replay across all spans.
// Tiles never share *BeatMap: NewPlayer scrubs objects in place.
// Tiles that fail (bad replay/map) are reported as skips so one bad apple
// never kills a 160-tile batch; callers drop those rows and continue.
func buildGridTiles(spec *GridSpec, beatmaps []*beatmap.BeatMap) ([]*GridTile, [][2]string) {
	seen := map[string]*Player{}
	labels := map[string]string{}
	users := map[string]string{}
	order := []string{}
	skips := [][2]string{}
	for _, span := range spec.Spans {
		for _, ts := range span.Tiles {
			if _, ok := seen[ts.Replay]; ok {
				continue
			}
			p, label, username, err := buildOneTile(ts.Replay, beatmaps)
			if err != nil {
				log.Printf("grid tile SKIP %s: %v", ts.Replay, err)
				skips = append(skips, [2]string{ts.Replay, err.Error()})
				continue
			}
			seen[ts.Replay] = p
			order = append(order, ts.Replay)
			labels[ts.Replay] = label
			users[ts.Replay] = username
			log.Printf("grid tile: %s", label)
		}
	}
	tiles := make([]*GridTile, 0, len(order))
	for _, rp := range order {
		tiles = append(tiles, &GridTile{player: seen[rp], replay: rp, label: labels[rp], username: users[rp]})
	}
	return tiles, skips
}

// cardUsername returns the carded player: first tile username wins; extra
// users are logged and omitted (single-player batches only).
func cardUsername(tiles []*GridTile) string {
	seen := []string{}
	for _, t := range tiles {
		if t.username == "" {
			continue
		}
		dup := false
		for _, u := range seen {
			if u == t.username {
				dup = true
				break
			}
		}
		if !dup {
			seen = append(seen, t.username)
		}
	}
	if len(seen) > 1 {
		log.Printf("grid card: %d users in batch, carding %q and omitting the rest", len(seen), seen[0])
	}
	if len(seen) == 0 {
		return ""
	}
	return seen[0]
}

// buildOneTile resolves and constructs a single tile player. NewPlayer can
// panic on malformed map data; recover per tile so the batch survives.
func buildOneTile(replayPath string, beatmaps []*beatmap.BeatMap) (p *Player, label, username string, err error) {
	defer func() {
		if r := recover(); r != nil {
			p = nil
			label = ""
			username = ""
			err = fmt.Errorf("construct: %v", r)
		}
	}()
	rp, bMap, err := resolveGridReplay(replayPath, beatmaps)
	if err != nil {
		return nil, "", "", err
	}
	svStart, svEnd, svSkip := settings.START, settings.END, settings.SKIP
	svKO := settings.KNOCKOUTREPLAYS
	defer func() {
		settings.START, settings.END, settings.SKIP = svStart, svEnd, svSkip
		settings.KNOCKOUTREPLAYS = svKO
	}()
	settings.KNOCKOUTREPLAYS = []string{replayPath}
	// Grid tiles always skip intros, like legacy -skip records: the clock
	// starts just before the first object (minus preempt) instead of the
	// lead-in. Probe durations and start offsets adapt automatically.
	settings.SKIP = true
	p = NewPlayer(bMap)
	if n := len(p.controller.GetCursors()); n != 1 {
		return nil, "", "", fmt.Errorf("want 1 cursor, got %d", n)
	}
	return p, bMap.Artist + " - " + bMap.Name + " [" + bMap.Difficulty + "]", rp.Username, nil
}

func resolveGridReplay(replayPath string, beatmaps []*beatmap.BeatMap) (*rplpa.Replay, *beatmap.BeatMap, error) {
	raw, err := os.ReadFile(replayPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read replay %s: %w", replayPath, err)
	}
	rp, err := rplpa.ParseReplay(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse replay %s: %w", replayPath, err)
	}
	if rp.PlayMode != 0 {
		return nil, nil, fmt.Errorf("non-standard replay: %s", replayPath)
	}
	var bMap *beatmap.BeatMap
	for _, b := range beatmaps {
		if strings.EqualFold(b.MD5, rp.BeatmapMD5) {
			bMap = b
			break
		}
	}
	if bMap == nil {
		return nil, nil, fmt.Errorf("no map for md5 %s", rp.BeatmapMD5)
	}
	// One BeatMap per TILE, never shared: ParseObjects/ParseTimingPoints
	// APPEND, and NewPlayer scrubs objects in place, so two tiles on one
	// map corrupt each other (duplicated objects, slider edge panics).
	bMap = bMap.Clone()
	modsParsed := difficulty2.Modifier(rp.Mods)
	if !modsParsed.Compatible() {
		return nil, nil, fmt.Errorf("incompatible mods in %s", replayPath)
	}
	bMap.Diff.SetMods(modsParsed)
	beatmap.ParseTimingPointsAndPauses(bMap)
	beatmap.ParseObjects(bMap, false, true)
	bMap.LoadCustomSamples()
	return rp, bMap, nil
}

// layoutTiles sizes tile cameras to their span rects. Spec rects are
// top-down (ffmpeg/Python convention); gl.Viewport is bottom-up, so Y is
// flipped here once per span (same-row layouts hid this for months).
func layoutTiles(canvasH int, tiles []*GridTile, tsp []GridTileSpec) {
	byReplay := map[string][4]int{}
	for _, ts := range tsp {
		byReplay[ts.Replay] = [4]int{ts.X, canvasH - ts.Y - ts.H, ts.W, ts.H}
	}
	sizeCameras(tiles, byReplay)
}

// layoutTilesMorph interpolates each tile between its from/to rects at
// progress p in [0,1] (top-down), then flips Y for GL like layoutTiles.
// Dying tiles ride their precomputed shrink-to-center rects. Called every
// video frame: cameras re-fit as boxes glide, exactly like a legacy glide
// morph but rendered live — no frozen time anywhere.
func layoutTilesMorph(canvasH int, tiles []*GridTile, tsp []GridTileSpec, p float64) {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	byReplay := map[string][4]int{}
	for _, ts := range tsp {
		x := float64(ts.Ax) + (float64(ts.Bx-ts.Ax) * p)
		y := float64(ts.Ay) + (float64(ts.By-ts.Ay) * p)
		w := float64(ts.Aw) + (float64(ts.Bw-ts.Aw) * p)
		h := float64(ts.Ah) + (float64(ts.Bh-ts.Ah) * p)
		xi, yi, wi, hi := int(math.Round(x)), int(math.Round(y)), int(math.Round(w)), int(math.Round(h))
		if wi < 1 {
			wi = 1
		}
		if hi < 1 {
			hi = 1
		}
		byReplay[ts.Replay] = [4]int{xi, canvasH - yi - hi, wi, hi}
	}
	sizeCameras(tiles, byReplay)
}

// sizeCameras fits every tile's cameras to its rect (GL coords).
func sizeCameras(tiles []*GridTile, byReplay map[string][4]int) {
	sc := settings.Playfield.Scale
	sbScale := 1.0
	if settings.Playfield.ScaleStoryboardWithPlayfield {
		sbScale = sc
	}
	for _, t := range tiles {
		r, ok := byReplay[t.replay]
		if !ok {
			continue
		}
		t.rect = r
		p := t.player
		p.mainCamera.SetOsuViewport(r[2], r[3], sc, true, settings.Playfield.OsuShift)
		p.mainCamera.Update()
		p.objectCamera.SetOsuViewport(r[2], r[3], sc, true, settings.Playfield.OsuShift)
		p.objectCamera.Update()
		// Cursor edge-bounce bounds follow the tile (not the full canvas).
		for _, c := range p.controller.GetCursors() {
			c.SetOsuRect(p.mainCamera.GetWorldRect())
		}
		p.bgCamera.SetOsuViewport(r[2], r[3], sbScale,
			!settings.Playfield.OsuShift && settings.Playfield.MoveStoryboardWithPlayfield, false)
		p.bgCamera.Update()
		p.uiCamera.SetViewport(r[2], r[3], true)
		p.uiCamera.SetViewportF(0, r[3], r[2], 0)
		p.uiCamera.Update()
	}
}

// gridCursorLogged gates one-shot cursor diagnostics.
var gridCursorLogged = false
var gridCursorLogged2 = false

// copyFile copies src to dst (os.Rename can't cross bind mounts).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ProbeResult is one tile's exact record length for timeline planning,
// or the construction error (Python marks those rows failed and continues
// with the rest instead of losing the batch).
type ProbeResult struct {
	Replay    string  `json:"replay"`
	DurationS float64 `json:"duration_s"`
	// StartOffsetMs is the tile clock at video zero (negative lead-in):
	// Python delays the map mp3 by -startOffset/rate so song zero meets
	// map zero instead of the video start.
	StartOffsetMs float64 `json:"start_offset_ms,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// ProbeGrid loads every distinct tile player and reports exact durations
// (MapEnd, the same length a legacy record would have). No recording, no
// frames. Must run construction on the main thread (GL).
func ProbeGrid(specPath string, beatmaps []*beatmap.BeatMap, outPath string) {
	spec, err := loadGridSpec(specPath)
	if err != nil {
		panic(err)
	}
	if len(beatmaps) == 0 {
		panic("grid: no beatmaps loaded")
	}
	var tiles []*GridTile
	var skips [][2]string
	var buildErr error
	goroutines.CallMain(func() {
		defer func() {
			if r := recover(); r != nil {
				buildErr = fmt.Errorf("grid probe: %v", r)
			}
		}()
		tiles, skips = buildGridTiles(spec, beatmaps)
	})
	if buildErr != nil {
		panic(buildErr)
	}
	results := make([]ProbeResult, 0, len(tiles)+len(skips))
	for _, t := range tiles {
		// MapEnd is nominal map time and the clock starts at startOffset
		// (lead-in); wall duration divides the span by the playback rate
		// (DT finishes early, HT late) — matching legacy record length.
		rate := t.player.bMap.Diff.GetSpeed()
		if rate <= 0 {
			rate = 1
		}
		results = append(results, ProbeResult{
			Replay:        t.replay,
			DurationS:     (t.player.MapEnd - t.player.startOffset) / 1000.0 / rate,
			StartOffsetMs: t.player.startOffset,
		})
	}
	for _, s := range skips {
		results = append(results, ProbeResult{Replay: s[0], Error: s[1]})
	}
	raw, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(outPath, raw, 0644); err != nil {
		panic(err)
	}
	log.Printf("grid probe: %d tiles -> %s", len(results), outPath)
}

// RunGrid renders every span in spec: one ffmpeg encode per span, clocks run
// continuously across spans (no re-seeks). Runs on the worker thread like
// mainLoopRecord: ticks and ffmpeg process management happen here, every GL
// touch (construction, draws, frame capture) goes through the main pump.
func RunGrid(specPath string, beatmaps []*beatmap.BeatMap) {
	spec, err := loadGridSpec(specPath)
	if err != nil {
		panic(err)
	}
	if len(beatmaps) == 0 {
		panic("grid: no beatmaps loaded")
	}
	fps := float64(spec.FPS)
	updateDelta := 1000.0 / math.Max(fps, 1000)
	fpsDelta := 1000.0 / fps

	// M2 look: cursors on (per-tile bounds below); storyboards off (dim
	// static BG per tile, no animated SB threads); bloom/blur follow the
	// loaded profile like legacy renders.
	settings.Playfield.DrawCursors = os.Getenv("GRID_NOCURSOR") == ""
	settings.Playfield.Background.LoadStoryboards = false

	// GL init block on the pump thread: shared FBO + all tile players.
	var fbo *buffer.Framebuffer
	byReplay := map[string]*GridTile{}
	var initErr error
	var built []*GridTile
	var skips [][2]string
	goroutines.CallMain(func() {
		defer func() {
			if r := recover(); r != nil {
				initErr = fmt.Errorf("grid init: %v", r)
			}
		}()
		// Offscreen target (mirrors mainLoopRecord): window size is
		// irrelevant, the spec canvas rules.
		fbo = buffer.NewFrameMultisampleScreen(spec.Width, spec.Height, false, 0)
		built, skips = buildGridTiles(spec, beatmaps)
		for _, s := range skips {
			log.Printf("grid tile SKIP %s: %v", s[0], s[1])
		}
		if len(built) == 0 {
			initErr = fmt.Errorf("grid init: no tiles constructed")
			return
		}
		for _, t := range built {
			byReplay[t.replay] = t
		}
		gridOv = buildGridOverlay(spec, cardUsername(built))
	})
	if initErr != nil {
		panic(initErr)
	}

	if err := os.MkdirAll(spec.OutDir, 0755); err != nil {
		panic(err)
	}

	// Terminal outro span (config duration, spec lines): rendered like any
	// other span so the outro joins the timeline with zero Python-side work.
	if spec.Outro != nil && settings.Grid.Outro.Enabled && settings.Grid.Outro.Duration > 0 &&
		(spec.Outro.Line1 != "" || spec.Outro.Line2 != "") {
		lastEnd := spec.Spans[len(spec.Spans)-1].End
		spec.Spans = append(spec.Spans, GridSpanSpec{
			Name: "seg-outro", Start: lastEnd,
			End:   lastEnd + settings.Grid.Outro.Duration,
			Kind:  "outro",
			Tiles: []GridTileSpec{{Replay: "__outro__"}},
		})
	}

	for si, span := range spec.Spans {
		spanMs := (span.End - span.Start) * 1000
		log.Printf("grid span %d/%d %s [%.1f, %.1f) %d tiles",
			si+1, len(spec.Spans), span.Name, span.Start, span.End, len(span.Tiles))
		outro := span.Kind == "outro"
		active := make([]*GridTile, 0, len(span.Tiles))
		if !outro {
			for _, ts := range span.Tiles {
				t, ok := byReplay[ts.Replay]
				if !ok || t.done {
					continue
				}
				active = append(active, t)
			}
			if len(active) == 0 {
				panic(fmt.Sprintf("grid span %s: no live tiles", span.Name))
			}
		}
		morph := span.Kind == "morph"
		if !morph && !outro {
			layoutTiles(spec.Height, active, span.Tiles)
		}
		var dying map[string]bool
		if morph {
			dying = make(map[string]bool, len(span.Tiles))
			for _, ts := range span.Tiles {
				if ts.Dying {
					dying[ts.Replay] = true
				}
			}
		}

		ffmpeg.StartVideoSpan(spec.FPS, spec.Width, spec.Height, span.Name)
		elapsed := 0.0
		deltaSumF := fpsDelta
		frames := int64(0)
		for elapsed < spanMs {
			if !outro {
				for _, t := range active {
					if t.done {
						continue
					}
					tickTile(t, updateDelta)
				}
			}
			elapsed += updateDelta
			deltaSumF += updateDelta
			if deltaSumF >= fpsDelta {
				if outro {
					drawGridOutroFrame(fbo, spec.Width, spec.Height, elapsed/spanMs)
				} else {
					if morph {
						layoutTilesMorph(spec.Height, active, span.Tiles, elapsed/spanMs)
					}
					drawGridFrame(fbo, active, spec.Width, spec.Height, dying, elapsed/spanMs)
				}
				deltaSumF -= fpsDelta
				frames++
				if os.Getenv("GRID_TRACE") != "" && frames%30 == 1 {
					for _, t := range active {
						p := t.player
						log.Printf("grid-trace %s frame=%d progressMsF=%.0f rawPos=%.0f mapEnd=%.0f diffSpeed=%.2f trackState=%d trackSpeed=%.2f",
							t.label, frames, p.progressMsF, p.rawPositionF, p.MapEnd,
							p.bMap.Diff.GetSpeed(), p.musicPlayer.GetState(), p.musicPlayer.GetSpeed())
					}
				}
			}
		}
		raw := ffmpeg.StopVideoSpan()
		final := filepath.Join(spec.OutDir, span.Name+".mp4")
		// Bind mounts count as separate filesystems: rename(2) fails
		// EXDEV across them, so copy + remove instead.
		if err := copyFile(raw, final); err != nil {
			panic(fmt.Sprintf("grid span %s: %v", span.Name, err))
		}
		_ = os.RemoveAll(filepath.Join(spec.OutDir, span.Name+"_temp"))
		log.Printf("grid span %s: %d frames -> %s", span.Name, frames, final)
	}
	// Dropped-tile manifest for the caller: tiles that panicked mid-record
	// are baked in as black cells past their failure point. Report, don't
	// hide: the batch composition already accounts for them.
	var failed []string
	for _, t := range byReplay {
		if t.failed {
			failed = append(failed, t.replay)
		}
	}
	sort.Strings(failed)
	if len(failed) > 0 {
		raw, err := json.MarshalIndent(failed, "", "  ")
		if err == nil {
			if err := os.WriteFile(filepath.Join(spec.OutDir, "grid-failed.json"), raw, 0644); err != nil {
				log.Printf("grid: failed manifest unwritable: %v", err)
			} else {
				log.Printf("grid: %d tile(s) dropped mid-record, see grid-failed.json", len(failed))
			}
		}
	}
	log.Println("grid: all spans rendered")
}

// drawGridFrame renders one grid frame into the span FBO and pushes it to
// the encoder. dying maps replay -> true for tiles shrinking out this
// morph; fadeP is the morph progress (fade ramps with it). Pump thread
// only (GL context).
func drawGridFrame(fbo *buffer.Framebuffer, active []*GridTile, width, height int, dying map[string]bool, fadeP float64) {
	goroutines.CallMain(func() {
		fbo.Bind()
		ffmpeg.PreFrame()
		viewport.Push(width, height)
		gl.ClearColor(0, 0, 0, 1)
		gl.Enable(gl.SCISSOR_TEST)
		gl.Disable(gl.DITHER)
		gl.Clear(gl.COLOR_BUFFER_BIT)
		for _, t := range active {
			if t.done {
				continue
			}
			viewport.PushPos(t.rect[0], t.rect[1], t.rect[2], t.rect[3])
			t.player.Draw(0)
			viewport.Pop()
		}
		if gridOv != nil {
			gridOv.batch.Begin()
			gridOv.batch.SetCamera(gridOv.camera)
			drawGridOverlay()
			if len(dying) > 0 && fadeP > 0 {
				for _, t := range active {
					if dying[t.replay] {
						drawTileFade(width, height, t.rect, float32(fadeP))
					}
				}
			}
			gridOv.batch.End()
		}
		viewport.Pop()
		ffmpeg.MakeFrame()
		fbo.Unbind()
	})
}

// drawGridOutroFrame renders one outro frame (black + stat lines with
// fades) at span progress p in [0,1]. Pump thread only (GL context).
func drawGridOutroFrame(fbo *buffer.Framebuffer, width, height int, p float64) {
	goroutines.CallMain(func() {
		fbo.Bind()
		ffmpeg.PreFrame()
		viewport.Push(width, height)
		gl.ClearColor(0, 0, 0, 1)
		gl.Enable(gl.SCISSOR_TEST)
		gl.Disable(gl.DITHER)
		gl.Clear(gl.COLOR_BUFFER_BIT)
		drawGridOutro(p)
		viewport.Pop()
		ffmpeg.MakeFrame()
		fbo.Unbind()
	})
}
