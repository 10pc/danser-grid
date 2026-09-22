package states

// Grid overlay suite: header bar, player card and outro rendered in-binary
// from spec strings (computed outside danser-grid). All sprites are built
// once per batch on the pump thread; per-frame work is a batch submit.
// Coordinates below are top-down canvas pixels, converted to the
// center-origin y-down ortho used for submission.

import (
	"fmt"
	"log"
	"strings"

	"github.com/go-gl/mathgl/mgl32"
	batch2 "github.com/wieku/danser-go/framework/graphics/batch"
	"github.com/wieku/danser-go/framework/graphics/font"
	"github.com/wieku/danser-go/framework/graphics/sprite"
	"github.com/wieku/danser-go/framework/graphics/texture"
	"github.com/wieku/danser-go/framework/math/color"
	"github.com/wieku/danser-go/framework/math/vector"
	"github.com/wieku/danser-go/app/settings"
)

type gridOverlayState struct {
	batch  *batch2.QuadBatch
	camera mgl32.Mat4

	header *sprite.TextSprite

	cardAvatar *sprite.Sprite
	cardName   *sprite.TextSprite
	cardSub    *sprite.TextSprite

	outro1 *sprite.TextSprite
	outro2 *sprite.TextSprite

	fadeTex *texture.TextureSingle
	fade    *sprite.Sprite
}

// gridOv is the active batch overlay (single grid run per process).
var gridOv *gridOverlayState

func parseGridColor(s string) color.Color {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		return color.NewRGB(1, 1, 1)
	}
	return color.NewRGB(float32(r)/255, float32(g)/255, float32(b)/255)
}

// buildGridOverlay constructs header/card/outro sprites. Pump thread only
// (textures + font atlas). Missing pieces degrade to text-only or skipped
// elements, never a failed batch.
func buildGridOverlay(spec *GridSpec, cardUser string) *gridOverlayState {
	fnt := font.GetFont("Quicksand Bold")
	if fnt == nil {
		log.Println("grid overlay: font missing, overlays off")
		return nil
	}
	w := float64(spec.Width)
	h := float64(spec.Height)
	ov := &gridOverlayState{}
	ov.batch = batch2.NewQuadBatch()
	ov.camera = mgl32.Ortho(float32(-w/2), float32(w/2), float32(h/2), float32(-h/2), 1, -1)
	ov.fadeTex = texture.NewTextureSingle(1, 1, 0)
	ov.fadeTex.SetData(0, 0, 1, 1, []uint8{255, 255, 255, 255})
	fadeReg := ov.fadeTex.GetRegion()
	ov.fade = sprite.NewSpriteSingle(&fadeReg, 1004, vector.NewVec2d(0, 0), vector.TopLeft)
	ov.fade.SetColor(color.NewRGB(0, 0, 0))

	cx := func(x float64) float64 { return x - w/2 }   // top-down px -> ortho
	cy := func(y float64) float64 { return y - h / 2 } // top-down px -> ortho

	if spec.Header != nil && settings.Grid.Header.Enabled && spec.Header.Line != "" {
		cfg := settings.Grid.Header
		line := strings.ReplaceAll(cfg.Template, "{line}", spec.Header.Line)
		ov.header = sprite.NewTextSpriteSize(line, fnt, float64(cfg.FontSize), 1001,
			vector.NewVec2d(0, cy(float64(cfg.Height)/2)), vector.TopCentre)
		ov.header.SetColor(parseGridColor(cfg.Color))
	}

	if spec.Player != nil && settings.Grid.Card.Enabled && spec.Player.Username != "" {
		cfg := settings.Grid.Card
		top := float64(settings.Grid.Header.Height) + float64(cfg.Y)
		if spec.Header == nil || !settings.Grid.Header.Enabled {
			top = float64(cfg.Y)
		}
		if cardUser != "" && cardUser != spec.Player.Username {
			log.Printf("grid card: spec user %q != tile user %q, using spec",
				spec.Player.Username, cardUser)
		}
		if spec.Player.Avatar != "" {
			if px, err := texture.NewPixmapFileString(spec.Player.Avatar); err != nil {
				log.Printf("grid card: avatar unreadable (%v), text-only card", err)
			} else {
				// No Dispose: upload timing is internal; one small texture
				// per batch is not worth a use-after-free gamble.
				tex := texture.LoadTextureSingle(px.RGBA(), 0)
				tw := float64(tex.GetWidth())
				if tw < 1 {
					tw = 1
				}
				reg := tex.GetRegion()
				av := sprite.NewSpriteSingle(&reg, 1001,
					vector.NewVec2d(cx(float64(cfg.X)), cy(top)), vector.TopLeft)
				av.SetScale(float64(cfg.AvatarSize) / tw)
				ov.cardAvatar = av
			}
		}
		tx := float64(cfg.X)
		if ov.cardAvatar != nil {
			tx += float64(cfg.AvatarSize) + 16
		}
		nameLine := spec.Player.Username
		subLine := ""
		if cfg.ShowCountry && spec.Player.Country != "" {
			nameLine += "  " + spec.Player.Country
		}
		if cfg.ShowRank && spec.Player.Rank != "" {
			subLine = spec.Player.Rank
		}
		ov.cardName = sprite.NewTextSpriteSize(nameLine, fnt, float64(cfg.NameSize), 1002,
			vector.NewVec2d(cx(tx), cy(top)), vector.TopLeft)
		if subLine != "" {
			ov.cardSub = sprite.NewTextSpriteSize(subLine, fnt, float64(cfg.SubSize), 1002,
				vector.NewVec2d(cx(tx), cy(top+float64(cfg.NameSize)+8)), vector.TopLeft)
		}
	}

	if spec.Outro != nil && settings.Grid.Outro.Enabled &&
		(spec.Outro.Line1 != "" || spec.Outro.Line2 != "") {
		cfg := settings.Grid.Outro
		if spec.Outro.Line1 != "" {
			ov.outro1 = sprite.NewTextSpriteSize(spec.Outro.Line1, fnt, float64(cfg.TitleSize), 1003,
				vector.NewVec2d(0, cy(h/2-180)), vector.TopCentre)
		}
		if spec.Outro.Line2 != "" {
			ov.outro2 = sprite.NewTextSpriteSize(spec.Outro.Line2, fnt, float64(cfg.SubSize), 1003,
				vector.NewVec2d(0, cy(h/2-60)), vector.TopCentre)
		}
	}
	return ov
}

// drawGridOverlay submits header + card for a content frame. The caller
// owns the batch (Begin + camera set); tiles are already drawn underneath.
func drawGridOverlay() {
	if gridOv == nil {
		return
	}
	if gridOv.header != nil {
		gridOv.header.Draw(0, gridOv.batch)
	}
	if gridOv.cardAvatar != nil {
		gridOv.cardAvatar.Draw(0, gridOv.batch)
	}
	if gridOv.cardName != nil {
		gridOv.cardName.Draw(0, gridOv.batch)
	}
	if gridOv.cardSub != nil {
		gridOv.cardSub.Draw(0, gridOv.batch)
	}
}

// drawTileFade covers a tile rect (GL bottom-up coords) with black at
// alpha for dying tiles mid-morph. Batch must be open (see drawGridFrame).
func drawTileFade(canvasW, canvasH int, rect [4]int, alpha float32) {
	if gridOv == nil || gridOv.fade == nil || alpha <= 0 {
		return
	}
	if alpha > 1 {
		alpha = 1
	}
	x := float64(rect[0] - canvasW/2)
	y := float64(canvasH-rect[1]-rect[3]) - float64(canvasH)/2
	gridOv.fade.SetPosition(vector.NewVec2d(x, y))
	gridOv.fade.SetScaleV(vector.NewVec2d(float64(rect[2]), float64(rect[3])))
	gridOv.fade.SetAlpha(alpha)
	gridOv.fade.Draw(0, gridOv.batch)
}

// drawGridOutro renders one outro frame at progress p in [0,1] with the
// configured fade in/out. Canvas is already cleared black by the caller.
func drawGridOutro(p float64) {
	if gridOv == nil {
		return
	}
	cfg := settings.Grid.Outro
	alpha := float32(1)
	if cfg.FadeIn > 0 {
		d := cfg.Duration
		fi := cfg.FadeIn / d
		fo := cfg.FadeOut / d
		a := 1.0
		if p < fi {
			a = p / fi
		}
		if p > 1-fo {
			a = (1 - p) / fo
		}
		if a < 0 {
			a = 0
		}
		if a > 1 {
			a = 1
		}
		alpha = float32(a)
	}
	gridOv.batch.Begin()
	gridOv.batch.SetCamera(gridOv.camera)
	if gridOv.outro1 != nil {
		gridOv.outro1.SetAlpha(alpha)
		gridOv.outro1.Draw(0, gridOv.batch)
	}
	if gridOv.outro2 != nil {
		gridOv.outro2.SetAlpha(alpha)
		gridOv.outro2.Draw(0, gridOv.batch)
	}
	gridOv.batch.End()
}
