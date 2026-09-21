package settings

var Grid = initGrid()

func initGrid() *grid {
	return &grid{
		Header: &gridHeader{
			Enabled:  true,
			Height:   80,
			FontSize: 36,
			Color:    "#FFFFFF",
			Template: "danser-grid | {date} | {maps}",
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
			X:           24,
			Y:           0,
			AvatarSize:  128,
			NameSize:    40,
			SubSize:     30,
			ShowRank:    true,
			ShowCountry: true,
		},
	}
}

// Grid overlay suite for -grid mode: header bar, player card and outro are
// rendered in-binary from spec strings (computed outside, e.g. by the
// completionist pipeline). Card.Y is measured from the bottom of the
// header bar; 0 docks the card directly under it.
type grid struct {
	Header *gridHeader
	Outro  *gridOutro
	Card   *gridCard
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
	AvatarSize  int64
	NameSize    int64
	SubSize     int64
	ShowRank    bool
	ShowCountry bool
}
