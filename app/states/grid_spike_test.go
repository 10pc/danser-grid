//go:build gridspike

// Spike slices 1+2: two Players (different maps) coexist, tick independently,
// and render side-by-side tiles into one framebuffer (screenshot proof).
// No cursor pass yet (slice 2b), default clock rate (slice 3).
//
// GL discipline: danser's goroutines package owns the main thread.
// Everything touching GL (window, context, textures, NewPlayer) runs inside
// goroutines.CallMain; pure logic (settings, DB, ticks) runs on the worker.
// t.Fatalf never fires inside CallMain (wrong goroutine) — errors propagate
// out and fail on the worker instead.
package states

import (
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-gl/gl/v3.3-core/gl"
	"github.com/go-gl/glfw/v3.3/glfw"
	"github.com/wieku/danser-go/app/beatmap"
	difficulty2 "github.com/wieku/danser-go/app/beatmap/difficulty"
	"github.com/wieku/danser-go/app/database"
	"github.com/wieku/danser-go/app/input"
	"github.com/wieku/danser-go/app/settings"
	"github.com/wieku/danser-go/app/utils"
	"github.com/wieku/danser-go/framework/assets"
	"github.com/wieku/danser-go/framework/bass"
	"github.com/wieku/danser-go/framework/env"
	"github.com/wieku/danser-go/framework/graphics/font"
	"github.com/wieku/danser-go/framework/graphics/shader"
	"github.com/wieku/danser-go/framework/graphics/viewport"
	"github.com/wieku/danser-go/framework/goroutines"
	"github.com/wieku/danser-go/framework/platform"
	"github.com/wieku/rplpa"
)

func mustEnvGL() (string, string, error) {
	songsDir := os.Getenv("SPIKE_SONGS")
	replays := os.Getenv("SPIKE_REPLAYS")
	if songsDir == "" || replays == "" {
		return "", "", fmt.Errorf("SPIKE_SONGS and SPIKE_REPLAYS must be set")
	}
	return songsDir, replays, nil
}

func TestGridDual(t *testing.T) {
	// The RunMain pump + every CallMain closure must execute on ONE OS
	// thread: the GL context is current per-thread, and a migrated pump
	// silently loses it (MAX_TEXTURE_SIZE reads 0, later compiles die).
	runtime.LockOSThread()
	goroutines.RunMain(func() {
		if err := gridDual(t); err != nil {
			t.Fatalf("slice1: %v", err)
		}
	})
}

func TestShaderRepeat(t *testing.T) {
	goroutines.RunMain(func() {
		if err := shaderRepeat(t); err != nil {
			t.Fatalf("shader-repeat: %v", err)
		}
	})
}

func shaderRepeat(t *testing.T) error {
	env.Init("danser")
	var glErr error
	goroutines.CallMain(func() {
		defer func() {
			if r := recover(); r != nil {
				debug.PrintStack()
				glErr = fmt.Errorf("setup: %v", r)
			}
		}()
		if err := glfw.Init(); err != nil {
			glErr = fmt.Errorf("glfw: %w", err)
			return
		}
		glfw.WindowHint(glfw.Visible, glfw.False)
		win, err := glfw.CreateWindow(1920, 1080, "gridspike", nil, nil)
		if err != nil {
			glErr = fmt.Errorf("window: %w", err)
			return
		}
		win.MakeContextCurrent()
		if err := platform.GLInit(false); err != nil {
			glErr = fmt.Errorf("gl: %w", err)
			return
		}
		assets.Init(true)
		src, err := assets.Open("assets/shaders/slidercaps.vsh")
		if err != nil {
			glErr = fmt.Errorf("asset: %w", err)
			return
		}
		raw, err := io.ReadAll(src)
		_ = src.Close()
		if err != nil {
			glErr = fmt.Errorf("read: %w", err)
			return
		}
		t.Logf("source bytes: %d", len(raw))
		trivial := "#version 330\nvoid main(){gl_Position = vec4(0.0);}\n"
		for i, src := range []string{trivial, string(raw), string(raw)} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						glErr = fmt.Errorf("compile %d panicked: %v", i, r)
					}
				}()
				s := shader.NewSource(src, shader.Vertex)
				_ = s
				t.Logf("compile %d: no panic", i)
				s.Dispose()
			}()
			if glErr != nil {
				return
			}
		}
		t.Logf("SHADER-REPEAT PASS")
	})
	return glErr
}

func gridDual(t *testing.T) error {
	songsDir, replaysEnv, err := mustEnvGL()
	if err != nil {
		return err
	}
	replays := strings.Split(replaysEnv, ":")
	if len(replays) < 1 || len(replays) > 2 {
		return fmt.Errorf("need 1-2 replays, got %d", len(replays))
	}

	env.Init("danser")
	settings.LoadSettings("")
	settings.RECORD = true
	settings.KNOCKOUT = true
	settings.DIVIDES = 1
	settings.TAG = 1
	// Slice 2: no cursor pass (per-tile cursor FBOs are slice 2b), no
	// fullscreen effects (agreed MVP-offs) — playfields, HUD and dim BG only.
	settings.Playfield.DrawCursors = false
	settings.Playfield.Bloom.Enabled = false
	settings.Playfield.Background.Blur.Enabled = false
	settings.General.OsuSongsDir = songsDir
	settings.Audio.OnlineOffset = false
	settings.Playfield.SeizureWarning.Enabled = false
	settings.Gameplay.ShowResultsScreen = false
	settings.Recording.FrameWidth = 1920
	settings.Recording.FrameHeight = 1080
	settings.Graphics.WindowWidth = 1920
	settings.Graphics.WindowHeight = 1080

	if err := database.Init(); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()
	beatmaps := database.LoadBeatmaps(true, nil)
	if len(beatmaps) == 0 {
		return fmt.Errorf("no beatmaps imported from %s", songsDir)
	}
	t.Logf("imported %d beatmaps", len(beatmaps))

	type tileSpec struct {
		bMap      *beatmap.BeatMap
		replay    string
		display   string
		modsTaken bool
	}
	specs := make([]tileSpec, 0, 2)
	for _, rpPath := range replays {
		raw, err := os.ReadFile(rpPath)
		if err != nil {
			return fmt.Errorf("read replay: %w", err)
		}
		rp, err := rplpa.ParseReplay(raw)
		if err != nil {
			return fmt.Errorf("parse replay: %w", err)
		}
		if rp.PlayMode != 0 {
			return fmt.Errorf("non-standard replay: %s", rpPath)
		}
		var bMap *beatmap.BeatMap
		for _, b := range beatmaps {
			if strings.EqualFold(b.MD5, rp.BeatmapMD5) {
				bMap = b
				break
			}
		}
		if bMap == nil {
			return fmt.Errorf("no map for md5 %s", rp.BeatmapMD5)
		}
		modsParsed := difficulty2.Modifier(rp.Mods)
		if !modsParsed.Compatible() {
			return fmt.Errorf("incompatible mods in %s", rpPath)
		}
		bMap.Diff.SetMods(modsParsed)
		beatmap.ParseTimingPointsAndPauses(bMap)
		beatmap.ParseObjects(bMap, false, true)
		bMap.LoadCustomSamples()
		t.Logf("parsed %s", rpPath)
		specs = append(specs, tileSpec{bMap, rpPath,
			bMap.Artist + " - " + bMap.Name + " [" + bMap.Difficulty + "]", false})
	}
	if len(specs) == 2 && specs[0].display == specs[1].display {
		return fmt.Errorf("tiles share a map; need different maps")
	}

	players := make([]*Player, len(specs))
	names := make([]string, len(specs))
	var glErr error
	t.Logf("entering CallMain for GL init + construction")
	goroutines.CallMain(func() {
		defer func() {
			if r := recover(); r != nil {
				debug.PrintStack()
				glErr = fmt.Errorf("gl init/construct: %v", r)
			}
		}()
		if err := glfw.Init(); err != nil {
			glErr = fmt.Errorf("glfw: %w", err)
			return
		}
		glfw.WindowHint(glfw.Visible, glfw.False)
		win, err := glfw.CreateWindow(1920, 1080, "gridspike", nil, nil)
		if err != nil {
			glErr = fmt.Errorf("window: %w", err)
			return
		}
		win.MakeContextCurrent()
		input.Win = win
		if err := platform.GLInit(false); err != nil {
			glErr = fmt.Errorf("gl: %w", err)
			return
		}
		assets.Init(true)
		ff, err := assets.Open("assets/fonts/Quicksand-Bold.ttf")
		if err != nil {
			glErr = fmt.Errorf("font asset: %w", err)
			return
		}
		font.LoadFont(ff)
		_ = ff.Close()
		bass.Init(true)

		for i, sp := range specs {
			t.Logf("constructing tile %d", i)
			svStart, svEnd, svSkip := settings.START, settings.END, settings.SKIP
			svKO := settings.KNOCKOUTREPLAYS
			settings.KNOCKOUTREPLAYS = []string{sp.replay}
			players[i] = NewPlayer(sp.bMap)
			t.Logf("constructed tile %d", i)
			settings.START, settings.END, settings.SKIP = svStart, svEnd, svSkip
			settings.KNOCKOUTREPLAYS = svKO
			names[i] = sp.display
		}
		t.Logf("leaving CallMain")
	})
	if glErr != nil {
		return glErr
	}
	for i := range players {
		if players[i] == nil {
			return fmt.Errorf("tile %d: nil player", i)
		}
		if n := len(players[i].controller.GetCursors()); n != 1 {
			return fmt.Errorf("tile %d (%s): want 1 cursor, got %d", i, names[i], n)
		}
		t.Logf("tile%d: %s", i, names[i])
	}

	times := make([]float64, len(players))
	for i, p := range players {
		times[i] = p.rawPositionF
	}
	t.Logf("start raws (ms): %v", times)
	t.Logf("SPEED=%v DiffGetSpeed=%v musicState=%v start=%v startPoint=%v startPointE=%v version=%v",
		settings.SPEED, players[0].bMap.Diff.GetSpeed(),
		players[0].musicPlayer.GetState(), players[0].start, players[0].startPoint,
		players[0].startPointE, players[0].bMap.Version)
	for i := 0; i < 600; i++ {
		for _, p := range players {
			if done := p.Update(1.0); done {
				return fmt.Errorf("a tile finished after %dms (maps are minutes long)", i)
			}
		}
		if i < 100 && i%10 == 9 {
			t.Logf("tick %d: raw=%.1f state=%v", i+1,
				players[0].rawPositionF, players[0].musicPlayer.GetState())
		}
		if i%100 == 99 {
			t.Logf("tick %d: raw=%.1f prog=%.1f state=%v", i+1,
				players[0].rawPositionF, players[0].progressMsF,
				players[0].musicPlayer.GetState())
		}
	}
	for i, p := range players {
		after := p.rawPositionF
		// 600 ticks of 1ms must advance the raw clock ~600ms. (GetTime lags
		// raw by oldOffset on old-format maps; raw is the exact quantity.)
		if after-times[i] < 590.0 {
			return fmt.Errorf("tile %d clock advanced only %.1fms over 600 ticks", i, after-times[i])
		}
		times[i] = after
	}
	t.Logf("progress after 600 ticks (ms): %v", times)
	t.Logf("SLICE1 PASS: %d Player(s) coexist with independent controllers and clocks", len(players))

	// ---- slice 2: static split-viewport grid + screenshot ----
	rects := [][4]int{{0, 0, 960, 1080}, {960, 0, 960, 1080}}
	if len(players) != len(rects) {
		return fmt.Errorf("spike grid supports exactly 2 tiles")
	}
	for i, p := range players {
		retile(p, rects[i])
	}
	// advance both clocks 15s in (gameplay active on both maps)
	for i := 0; i < 14400; i++ {
		for _, p := range players {
			if done := p.Update(1.0); done {
				return fmt.Errorf("a tile finished early at tick %d", i)
			}
		}
	}
	t.Logf("clocks at shot: %.0f / %.0f", players[0].GetTime(), players[1].GetTime())
	// Readback sanity: solid red, no draws — isolates glReadPixels path.
	gl.ClearColor(1, 0, 0, 1)
	gl.Disable(gl.DITHER)
	gl.Clear(gl.COLOR_BUFFER_BIT)
	gl.Finish()
	utils.MakeScreenshot(1920, 1080, "grid-red", false)
	redShot := filepath.Join(env.DataDir(), "screenshots", "grid-red.png")
	rf, err := os.Open(redShot)
	if err != nil {
		return fmt.Errorf("red screenshot missing: %w", err)
	}
	redImg, err := png.Decode(rf)
	_ = rf.Close()
	if err != nil {
		return fmt.Errorf("red decode: %w", err)
	}
	rr, _, _, _ := redImg.At(960, 540).RGBA()
	t.Logf("red probe pixel: R=%d", rr>>8)
	gl.ClearColor(0, 0, 0, 1)
	gl.Enable(gl.SCISSOR_TEST)
	gl.Disable(gl.DITHER)
	gl.Clear(gl.COLOR_BUFFER_BIT)
	for i, p := range players {
		r := rects[i]
		viewport.PushPos(r[0], r[1], r[2], r[3])
		p.Draw(0)
		viewport.Pop()
	}
	gl.Finish()
	utils.MakeScreenshot(1920, 1080, "grid-proof", false)
	shot := filepath.Join(env.DataDir(), "screenshots", "grid-proof.png")
	if err := assertGridShot(t, shot); err != nil {
		return err
	}
	t.Logf("SLICE2 PASS: split-viewport grid renders two distinct playfields")
	return nil
}

// retile sizes a player's cameras to an absolute tile rect (pixels).
func retile(p *Player, r [4]int) {
	_, _, w, h := r[0], r[1], r[2], r[3]
	sc := settings.Playfield.Scale
	p.mainCamera.SetOsuViewport(w, h, sc, true, settings.Playfield.OsuShift)
	p.mainCamera.Update()
	p.objectCamera.SetOsuViewport(w, h, sc, true, settings.Playfield.OsuShift)
	p.objectCamera.Update()
	sbScale := 1.0
	if settings.Playfield.ScaleStoryboardWithPlayfield {
		sbScale = sc
	}
	p.bgCamera.SetOsuViewport(w, h, sbScale,
		!settings.Playfield.OsuShift && settings.Playfield.MoveStoryboardWithPlayfield, false)
	p.bgCamera.Update()
	p.uiCamera.SetViewport(w, h, true)
	p.uiCamera.SetViewportF(0, h, w, 0)
	p.uiCamera.Update()
}

// assertGridShot checks a 1920x1080 PNG: both halves non-black with content,
// and the halves differ (different maps).
func assertGridShot(t *testing.T, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("screenshot missing: %w", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	b := img.Bounds()
	if b.Dx() != 1920 || b.Dy() != 1080 {
		return fmt.Errorf("want 1920x1080, got %dx%d", b.Dx(), b.Dy())
	}
	stats := func(x0, x1 int) (mean, variance float64) {
		var n int64
		var sum, sum2 float64
		for y := 0; y < b.Dy(); y += 7 {
			for x := x0; x < x1; x += 7 {
				rr, gg, bb, _ := img.At(x, y).RGBA()
				v := float64(rr+gg+bb) / (3 * 257)
				sum += v
				sum2 += v * v
				n++
			}
		}
		mean = sum / float64(n)
		variance = sum2/float64(n) - mean*mean
		return mean, variance
	}
	mL, vL := stats(0, 960)
	mR, vR := stats(960, 1920)
	t.Logf("halves mean/var: L=%.1f/%.0f R=%.1f/%.0f", mL, vL, mR, vR)
	if mL < 3 || mR < 3 {
		return fmt.Errorf("a half is (near-)black: means %.1f %.1f", mL, mR)
	}
	if vL < 20 || vR < 20 {
		return fmt.Errorf("a half is flat: variances %.0f %.0f", vL, vR)
	}
	if d := mL - mR; d < -30 || d > 30 {
		t.Logf("halves differ strongly (%.1f), fine", d)
	} else if d > -2 && d < 2 {
		return fmt.Errorf("halves suspiciously identical (means %.1f %.1f)", mL, mR)
	}
	return nil
}
