package ffmpeg

import (
	"os"
	"path/filepath"

	"github.com/wieku/danser-go/app/settings"
)

// Video-only span encodes for grid mode (danser-grid). Audio is mixed
// downstream from map mp3s, so spans skip the audio pipe and the mux.
//
// Lifecycle per span: StartVideoSpan (ffmpeg process + fresh write queue,
// reused PBO pool) ... frames ... StopVideoSpan (drain, process exit).
// The returned path is the raw span video; callers move it into place.

func spanTempDir() string {
	return filepath.Join(settings.Recording.GetOutputDir(), output+"_temp")
}

// StartVideoSpan begins a video-only span encode writing to
// <outdir>/<name>_temp/video.<container>.
func StartVideoSpan(fps, w, h int, name string) {
	preCheck()

	output = name

	_ = os.RemoveAll(spanTempDir())

	err := os.MkdirAll(spanTempDir(), 0755)
	if err != nil && !os.IsExist(err) {
		panic(err)
	}

	startVideo(fps, w, h)
}

// StopVideoSpan finishes the span encode and returns the span video path.
func StopVideoSpan() string {
	stopVideo()

	return filepath.Join(spanTempDir(), "video."+settings.Recording.Container)
}
