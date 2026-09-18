//go:build gridspike

// Spike slice 1: two Players (different maps) coexist in one process.
// Construction-only proof: shared atlas, independent controllers/clocks.
// No drawing yet (slice 2), default clock rate (slice 3).
//
// GL discipline: danser's goroutines package owns the main thread.
// Everything touching GL (window, context, textures, NewPlayer) runs inside
// goroutines.CallMain; pure logic (settings, DB, ticks) runs on the worker.
// t.Fatalf never fires inside CallMain (wrong goroutine) — errors propagate
// out and fail on the worker instead.
package states

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-gl/glfw/v3.3/glfw"
	"github.com/wieku/danser-go/app/beatmap"
	difficulty2 "github.com/wieku/danser-go/app/beatmap/difficulty"
	"github.com/wieku/danser-go/app/database"
	"github.com/wieku/danser-go/app/settings"
	"github.com/wieku/danser-go/framework/assets"
	"github.com/wieku/danser-go/framework/bass"
	"github.com/wieku/danser-go/framework/goroutines"
	"github.com/wieku/danser-go/framework/graphics/font"
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
	goroutines.RunMain(func() {
		if err := gridDual(t); err != nil {
			t.Fatalf("slice1: %v", err)
		}
	})
}

func gridDual(t *testing.T) error {
	songsDir, replaysEnv, err := mustEnvGL()
	if err != nil {
		return err
	}
	replays := strings.Split(replaysEnv, ":")
	if len(replays) != 2 {
		return fmt.Errorf("need exactly 2 replays, got %d", len(replays))
	}

	settings.LoadSettings("")
	settings.RECORD = true
	settings.KNOCKOUT = true
	settings.DIVIDES = 1
	settings.TAG = 1
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
		specs = append(specs, tileSpec{bMap, rpPath,
			bMap.Artist + " - " + bMap.Name + " [" + bMap.Difficulty + "]", false})
	}
	if specs[0].display == specs[1].display {
		return fmt.Errorf("tiles share a map; need different maps")
	}

	var players [2]*Player
	var names [2]string
	var glErr error
	goroutines.CallMain(func() {
		defer func() {
			if r := recover(); r != nil {
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
			svStart, svEnd, svSkip := settings.START, settings.END, settings.SKIP
			svKO := settings.KNOCKOUTREPLAYS
			settings.KNOCKOUTREPLAYS = []string{sp.replay}
			players[i] = NewPlayer(sp.bMap)
			settings.START, settings.END, settings.SKIP = svStart, svEnd, svSkip
			settings.KNOCKOUTREPLAYS = svKO
			names[i] = sp.display
		}
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
	}
	t.Logf("tile0: %s", names[0])
	t.Logf("tile1: %s", names[1])

	for i := 0; i < 600; i++ {
		for _, p := range players {
			if done := p.Update(1.0); done {
				return fmt.Errorf("a tile finished after %dms (maps are minutes long)", i)
			}
		}
	}
	t0, t1 := players[0].GetTime(), players[1].GetTime()
	t.Logf("progress after 600 ticks: %.1fms / %.1fms", t0, t1)
	if t0 <= 0 || t1 <= 0 {
		return fmt.Errorf("clocks did not advance")
	}
	t.Logf("SLICE1 PASS: two Players coexist with independent controllers and clocks")
	return nil
}
