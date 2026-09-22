package ffmpeg

import (
	"os"
	"path/filepath"

	"github.com/wieku/danser-go/app/settings"
)

// Video+audio span encodes for grid mode (danser-grid). Video carries the
// tile grid; audio carries the shared mixer output for the span — music
// tracks are virtual (silent), so this is effectively the hitsound bed,
// timed to gameplay. Python concats span audios and mixes them under its
// mp3 music wall.
//
// Lifecycle per span: StartVideoSpan (ffmpeg processes + fresh write
// queues, reused PBO pool) ... frames+audio ... StopVideoSpan/StopAudioSpan
// (drain, processes exit). Callers move both outputs into place (bind
// mounts defeat rename(2) across filesystems).
const gridAudioFPS = 1000.0

func spanTempDir() string {
	return filepath.Join(settings.Recording.GetOutputDir(), output+"_temp")
}

// StartVideoSpan begins a span encode writing to
// <outdir>/<name>_temp/video.<container> (+ audio.<container>).
func StartVideoSpan(fps, w, h int, name string) {
	preCheck()

	output = name

	_ = os.RemoveAll(spanTempDir())

	err := os.MkdirAll(spanTempDir(), 0755)
	if err != nil && !os.IsExist(err) {
		panic(err)
	}

	startVideo(fps, w, h)
	startAudio(gridAudioFPS)
}

// StopVideoSpan finishes the span video encode and returns its path.
func StopVideoSpan() string {
	stopVideo()

	return filepath.Join(spanTempDir(), "video."+settings.Recording.Container)
}

// StopVideoSpanAudio finishes the span audio encode and returns its path.
// Call after StopVideoSpan; the mixer output covers exactly the recorded
// span window (callers push per tick, silence included).
func StopVideoSpanAudio() string {
	stopAudio()

	return filepath.Join(spanTempDir(), "audio."+settings.Recording.Container)
}
