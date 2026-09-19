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

// GridTileSpec is one tile's placement (absolute canvas pixels) + source.
type GridTileSpec struct {
	Replay string `json:"replay"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	W      int    `json:"w"`
	H      int    `json:"h"`
}

// GridSpanSpec is one static span: full-layout video for [start, end) in
// grid (wall-clock) seconds.
type GridSpanSpec struct {
	Name  string         `json:"name"`
	Start float64        `json:"start"`
	End   float64        `json:"end"`
	Tiles []GridTileSpec `json:"tiles"`
}

// GridSpec is one batch: shared canvas, spans in time order.
type GridSpec struct {
	Width  int            `json:"width"`
	Height int            `json:"height"`
	FPS    int            `json:"fps"`
	OutDir string         `json:"outDir"`
	Spans  []GridSpanSpec `json:"spans"`
}

// GridTile is a live tile: its player plus source identity.
type GridTile struct {
	player *Player
	replay string
	label  string
	rect   [4]int
	done   bool
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
		for j, t := range s.Tiles {
			if t.W <= 0 || t.H <= 0 || t.Replay == "" {
				return nil, fmt.Errorf("grid spec: span %d tile %d bad rect/replay", i, j)
			}
		}
	}
	return &spec, nil
}

// buildGridTiles constructs one Player per DISTINCT replay across all spans.
// Tiles never share *BeatMap: NewPlayer scrubs objects in place.
func buildGridTiles(spec *GridSpec, beatmaps []*beatmap.BeatMap) ([]*GridTile, error) {
	seen := map[string]*Player{}
	labels := map[string]string{}
	order := []string{}
	for _, span := range spec.Spans {
		for _, ts := range span.Tiles {
			if _, ok := seen[ts.Replay]; ok {
				continue
			}
			_, bMap, err := resolveGridReplay(ts.Replay, beatmaps)
			if err != nil {
				return nil, err
			}
			svStart, svEnd, svSkip := settings.START, settings.END, settings.SKIP
			svKO := settings.KNOCKOUTREPLAYS
			settings.KNOCKOUTREPLAYS = []string{ts.Replay}
			p := NewPlayer(bMap)
			settings.START, settings.END, settings.SKIP = svStart, svEnd, svSkip
			settings.KNOCKOUTREPLAYS = svKO
			if n := len(p.controller.GetCursors()); n != 1 {
				return nil, fmt.Errorf("tile %s: want 1 cursor, got %d", ts.Replay, n)
			}
			seen[ts.Replay] = p
			order = append(order, ts.Replay)
			labels[ts.Replay] = bMap.Artist + " - " + bMap.Name + " [" + bMap.Difficulty + "]"
			log.Printf("grid tile: %s", labels[ts.Replay])
		}
	}
	tiles := make([]*GridTile, 0, len(order))
	for _, rp := range order {
		tiles = append(tiles, &GridTile{player: seen[rp], replay: rp, label: labels[rp]})
	}
	return tiles, nil
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

// layoutTiles sizes tile cameras to their span rects.
func layoutTiles(tiles []*GridTile, tsp []GridTileSpec) {
	byReplay := map[string][4]int{}
	for _, ts := range tsp {
		byReplay[ts.Replay] = [4]int{ts.X, ts.Y, ts.W, ts.H}
	}
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
	goroutines.CallMain(func() {
		defer func() {
			if r := recover(); r != nil {
				initErr = fmt.Errorf("grid init: %v", r)
			}
		}()
		// Offscreen target (mirrors mainLoopRecord): window size is
		// irrelevant, the spec canvas rules.
		fbo = buffer.NewFrameMultisampleScreen(spec.Width, spec.Height, false, 0)
		built, err := buildGridTiles(spec, beatmaps)
		if err != nil {
			initErr = err
			return
		}
		for _, t := range built {
			byReplay[t.replay] = t
		}
	})
	if initErr != nil {
		panic(initErr)
	}

	if err := os.MkdirAll(spec.OutDir, 0755); err != nil {
		panic(err)
	}

	for si, span := range spec.Spans {
		spanMs := (span.End - span.Start) * 1000
		log.Printf("grid span %d/%d %s [%.1f, %.1f) %d tiles",
			si+1, len(spec.Spans), span.Name, span.Start, span.End, len(span.Tiles))
		active := make([]*GridTile, 0, len(span.Tiles))
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
		layoutTiles(active, span.Tiles)

		ffmpeg.StartVideoSpan(spec.FPS, spec.Width, spec.Height, span.Name)
		elapsed := 0.0
		deltaSumF := fpsDelta
		frames := int64(0)
		for elapsed < spanMs {
			for _, t := range active {
				if t.done {
					continue
				}
				if t.player.Update(updateDelta) {
					t.done = true
				}
			}
			elapsed += updateDelta
			deltaSumF += updateDelta
			if deltaSumF >= fpsDelta {
				drawGridFrame(fbo, active, spec.Width, spec.Height)
				deltaSumF -= fpsDelta
				frames++
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
	log.Println("grid: all spans rendered")
}

// drawGridFrame renders one grid frame into the span FBO and pushes it to
// the encoder. Pump thread only (GL context).
func drawGridFrame(fbo *buffer.Framebuffer, active []*GridTile, width, height int) {
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
		viewport.Pop()
		ffmpeg.MakeFrame()
		fbo.Unbind()
	})
}
