package settings

var Grid = initGrid()

func initGrid() *grid {
	return &grid{
		// ScoreMode: "combined" (header total only), "tiles" (per-tile
		// only), "both". Combo and accuracy always stay per-tile.
		ScoreMode: "combined",
		Header: &gridHeader{
			Enabled:  true,
			Height:   80,
			FontSize: 36,
			Color:    "#FFFFFF",
			Template: "danser-grid | {line}",
		},
		Outro: &gridOutro{
			Enabled:   true,
			Duration:  6.0,
			TitleSize: 96,
			SubSize:   54,
			FadeIn:    1.0,
			FadeOut:   1.0,
		},
		Card: &gridCard{
			Enabled:     true,
			X:           16,
			Y:           0,
			ShowRank:    true,
			ShowCountry: true,
		},
	}
}

// Grid overlay suite for -grid mode: the header bar holds the player card
// (left), the title (center) and the running batch total (right); the outro
// closes the video. All strings come from the spec (computed outside,
// e.g. by the completionist pipeline). Card geometry derives from the
// header height; Card X/Y offset it from the bar's top-left.
type grid struct {
	ScoreMode string
	Header    *gridHeader
	Outro     *gridOutro
	Card      *gridCard
}

type gridHeader struct {
	Enabled  bool
	Height   int64
	FontSize int64
	Color    string
	Template string
}

type gridOutro struct {
	Enabled   bool
	Duration  float64
	TitleSize int64
	SubSize   int64
	FadeIn    float64
	FadeOut   float64
}

type gridCard struct {
	Enabled     bool
	X           int64
	Y           int64
	ShowRank    bool
	ShowCountry bool
}
