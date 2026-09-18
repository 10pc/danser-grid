//go:build gridspike

// Spike slice 1: two Players (different maps) coexist in one process.
// Construction-only proof: shared atlas, independent controllers/clocks.
// No drawing yet (slice 2), default clock rate (slice 3).
//
// Run (on unit-02, Xvfb required):
//   SPIKE_SONGS=/tmp/spike-songs SPIKE_REPLAYS=/tmp/r1.osr:/tmp/r2.osr \
//     go test -tags gridspike ./app/states/ -run TestGridDual -v -count=1
package states

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/go-gl/glfw/v3.3/glfw"
	"github.com/wieku/danser-go/app/beatmap"
	difficulty2 "github.com/wieku/danser-go/app/beatmap/difficulty"
	"github.com/wieku/danser-go/app/database"
	"github.com/wieku/danser-go/app/settings"
	"github.com/wieku/danser-go/framework/assets"
	"github.com/wieku/danser-go/framework/bass"
	"github.com/wieku/danser-go/framework/graphics/font"
	"github.com/wieku/danser-go/framework/platform"
	"github.com/wieku/rplpa"
)

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("env %s not set", key)
	}
	return v
}

func TestGridDual(t *testing.T) {
	runtime.LockOSThread()

	songsDir := mustEnv(t, "SPIKE_SONGS")
	replays := strings.Split(mustEnv(t, "SPIKE_REPLAYS"), ":")
	if len(replays) != 2 {
		t.Fatalf("need exactly 2 replays, got %d", len(replays))
	}

	// --- minimal app.go init sequence (record-mode semantics) ---
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

	if err := glfw.Init(); err != nil {
		t.Fatalf("glfw: %v", err)
	}
	defer glfw.Terminate()
	glfw.WindowHint(glfw.Visible, glfw.False)
	win, err := glfw.CreateWindow(1920, 1080, "gridspike", nil, nil)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	win.MakeContextCurrent()
	if err := platform.GLInit(false); err != nil {
		t.Fatalf("gl: %v", err)
	}

	assets.Init(true)
	ff, err := assets.Open("assets/fonts/Quicksand-Bold.ttf")
	if err != nil {
		t.Fatalf("font asset: %v", err)
	}
	font.LoadFont(ff)
	_ = ff.Close()

	bass.Init(true)

	if err := database.Init(); err != nil {
		t.Fatalf("database: %v", err)
	}
	defer database.Close()
	beatmaps := database.LoadBeatmaps(true, nil)
	if len(beatmaps) == 0 {
		t.Fatalf("no beatmaps imported from %s", songsDir)
	}
	t.Logf("imported %d beatmaps", len(beatmaps))

	// --- construct one Player per replay (different maps) ---
	type tile struct {
		player *Player
		name   string
	}
	tiles := make([]tile, 0, 2)
	for _, rpPath := range replays {
		raw, err := os.ReadFile(rpPath)
		if err != nil {
			t.Fatalf("read replay: %v", err)
		}
		rp, err := rplpa.ParseReplay(raw)
		if err != nil {
			t.Fatalf("parse replay: %v", err)
		}
		if rp.PlayMode != 0 {
			t.Fatalf("non-standard replay: %s", rpPath)
		}
		var bMap *beatmap.BeatMap
		for _, b := range beatmaps {
			if strings.EqualFold(b.MD5, rp.BeatmapMD5) {
				bMap = b
				break
			}
		}
		if bMap == nil {
			t.Fatalf("no map for md5 %s", rp.BeatmapMD5)
		}

		modsParsed := difficulty2.Modifier(rp.Mods)
		if !modsParsed.Compatible() {
			t.Fatalf("incompatible mods in %s", rpPath)
		}
		bMap.Diff.SetMods(modsParsed)
		beatmap.ParseTimingPointsAndPauses(bMap)
		beatmap.ParseObjects(bMap, false, true)
		bMap.LoadCustomSamples()

		// NewPlayer mutates globals (START, PLAYERS via controller) —
// save/restore so tiles don't see each other's values.
		svStart, svEnd, svSkip := settings.START, settings.END, settings.SKIP
		svKO := settings.KNOCKOUTREPLAYS
		settings.KNOCKOUTREPLAYS = []string{rpPath}
		p := NewPlayer(bMap)
		settings.START, settings.END, settings.SKIP = svStart, svEnd, svSkip
		settings.KNOCKOUTREPLAYS = svKO

		cursors := p.controller.GetCursors()
		if len(cursors) != 1 {
			t.Fatalf("tile %s: want 1 cursor, got %d", rpPath, len(cursors))
		}
		tiles = append(tiles, tile{p, bMap.Artist + " - " + bMap.Name + " [" + bMap.Difficulty + "]"})
	}
	if tiles[0].name == tiles[1].name {
		t.Fatalf("tiles share a map; need different maps")
	}
	t.Logf("tile0: %s", tiles[0].name)
	t.Logf("tile1: %s", tiles[1].name)

	// --- tick both clocks; neither may finish, panic, or interfere ---
	for i := 0; i < 600; i++ {
		for _, tl := range tiles {
			if done := tl.player.Update(1.0); done {
				t.Fatalf("tile %s finished after %dms (maps are minutes long)", tl.name, i)
			}
		}
	}
	t0, t1 := tiles[0].player.GetTime(), tiles[1].player.GetTime()
	t.Logf("progress after 600 ticks: %.1fms / %.1fms", t0, t1)
	if t0 <= 0 || t1 <= 0 {
		t.Fatalf("clocks did not advance")
	}
	t.Logf("SLICE1 PASS: two Players coexist with independent controllers and clocks")
}
