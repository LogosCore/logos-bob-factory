package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"slices"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// screenshot takes a picture of the host's screen (docs:
// contracts/task-results-and-actions.md, screenshot): every screen in one
// picture (screen 0), or one of them (1 its main screen, then the others in
// the host's order). Windows': the desktop of the session the minion runs
// in, by GDI (screenshot_windows.go); elsewhere an X display's (x11.go). The
// picture, a PNG or a JPEG, goes to core as a download's file does, with a
// name and no path: it is on no file system. The task ends with the
// screenshot data once core has every byte.

// shot is a picture taken: of which screen (display, the host's name for
// it; empty for every screen), and how many screens the host has (0: not
// asked).
type shot struct {
	img     *image.RGBA
	display string
	screens int
}

// monitor is one of the host's screens: where it is on the desktop, in
// pixels, and its name; primary, its main one.
type monitor struct {
	name       string
	x, y, w, h int
	primary    bool
}

// takeShot takes a picture of screen n, of X display xDisplay where there
// is one: the host's own way (tests swap it).
var takeShot = capture

// defaultJPEGQuality is a JPEG's when the task says none.
const defaultJPEGQuality = 85

func (r *runner) screenshot(ctx context.Context, t *task, args protocol.Args) {
	n, _ := protocol.ArgInt(args, screenshotScreen)
	format, _ := protocol.ArgString(args, screenshotFormat)
	quality, ok := protocol.ArgInt(args, screenshotQuality)
	if !ok {
		quality = defaultJPEGQuality
	}
	xDisplay, _ := protocol.ArgString(args, screenshotXDisplay)
	if n < 0 {
		r.end(t, false, nil, "screen is 0 for every screen, or a screen's number from 1")
		return
	}
	s, err := takeShot(int(n), xDisplay)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	body, ext, err := encodeShot(s.img, format, int(quality))
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	name := "screenshot-" + time.Now().Format("20060102-150405") + "." + ext
	x := newTransfer(name, bytes.NewReader(body), uint64(len(body)), false)
	if failed := r.sendTransfer(ctx, t, x); failed != "" {
		r.end(t, false, nil, failed)
		return
	}
	w, h := s.img.Bounds().Dx(), s.img.Bounds().Dy()
	r.write(t, []byte(describeShot(s, int(n), ext, len(body))+"\n"), protocol.Screenshot{Xfer: x.id, Size: x.total,
		SHA256: x.sum.Sum(nil), Width: uint64(w), Height: uint64(h), Display: s.display})
	r.end(t, true, nil, "")
}

// describeShot is what the task's output says of a picture: which screen,
// its size, its format and bytes.
func describeShot(s *shot, n int, ext string, size int) string {
	which := "every screen"
	if n > 0 {
		which = fmt.Sprintf("screen %d", n)
		if s.screens > 0 {
			which += fmt.Sprintf(" of %d", s.screens)
		}
		if s.display != "" {
			which += " (" + s.display + ")"
		}
	}
	kind := map[string]string{"png": "a PNG", "jpg": "a JPEG"}[ext]
	b := s.img.Bounds()
	return fmt.Sprintf("%s: %d × %d pixels, %s of %d bytes", which, b.Dx(), b.Dy(), kind, size)
}

// encodeShot is a picture as a file: a PNG (format png, or none) or a JPEG
// of quality 1 to 100; and the file's extension.
func encodeShot(img image.Image, format string, quality int) ([]byte, string, error) {
	var b bytes.Buffer
	switch format {
	case "", "png":
		if err := png.Encode(&b, img); err != nil {
			return nil, "", err
		}
		return b.Bytes(), "png", nil
	case "jpeg":
		if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: min(max(quality, 1), 100)}); err != nil {
			return nil, "", err
		}
		return b.Bytes(), "jpg", nil
	}
	return nil, "", fmt.Errorf("format %q: png or jpeg", format)
}

// pickScreen is screen n (from 1) of the host's screens: its main one
// first (the first when it names none), then the others in its order.
func pickScreen(ms []monitor, n int) (monitor, error) {
	if len(ms) == 0 {
		return monitor{}, errors.New("the host does not say which screens it has: take screen 0, every screen")
	}
	main := max(slices.IndexFunc(ms, func(m monitor) bool { return m.primary }), 0)
	order := append([]monitor{ms[main]}, slices.Delete(slices.Clone(ms), main, main+1)...)
	if n > len(order) {
		return monitor{}, fmt.Errorf("screen %d: the host has %d", n, len(order))
	}
	return order[n-1], nil
}
