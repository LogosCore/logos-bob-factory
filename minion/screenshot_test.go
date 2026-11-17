package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// testPicture is a w × h picture whose pixels say where they are.
func testPicture(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(4*x), byte(4*y), byte(x+y), 255
		}
	}
	return img
}

// swapShot makes takeShot answer with s (or err) for the test, and says
// what it was asked.
func swapShot(t *testing.T, s *shot, err error) *[2]any {
	t.Helper()
	asked := &[2]any{}
	takeShot = func(n int, d string) (*shot, error) {
		asked[0], asked[1] = n, d
		return s, err
	}
	t.Cleanup(func() { takeShot = capture })
	return asked
}

func startShot(t *testing.T, args protocol.Args) *runner {
	t.Helper()
	r := newRunner(description{Ops: []uint64{opScreenshot}}, &schedule{interval: time.Second})
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{6}, 16), Cmd: opScreenshot, Args: args})
	return r
}

// A screenshot goes as a download's file does, in chunks acked by core, its
// first naming it alone (no path: the picture is on no file system). Once
// core has every byte the task ends with the screenshot data: the
// picture's size in pixels, its screen, its SHA-256.
func TestScreenshot(t *testing.T) {
	asked := swapShot(t, &shot{img: testPicture(64, 48), display: "HDMI-1", screens: 2}, nil)
	r := startShot(t, protocol.Args{screenshotScreen: int64(1), screenshotFormat: "png", screenshotXDisplay: ":1"})

	var got []byte
	var first protocol.ChunkItem
	for first.Total == nil || uint64(len(got)) < *first.Total {
		items := nextMessage(t, r)
		r.delivered(items)
		c, _, ok := chunkIn(items)
		if !ok {
			continue
		}
		if c.Off == 0 {
			first = c
		}
		got = append(got, c.Data...)
		next := uint64(len(got))
		r.take(coreSays(t, protocol.AckItem{T: protocol.ItemAck, Xfer: c.Xfer, Next: &next}), items)
	}
	task := waitEnd(t, r, 2*time.Second)
	if !task.ok || len(task.chunks) != 1 {
		t.Fatalf("ended %+v", task)
	}
	s, err := protocol.ReadScreenshot(0, asCore(t, protocol.ResultScreenshot, task.chunks[0]))
	sum := sha256.Sum256(got)
	if err != nil || s.Width != 64 || s.Height != 48 || s.Display != "HDMI-1" || s.Size != uint64(len(got)) ||
		!bytes.Equal(s.SHA256, sum[:]) || !bytes.Equal(s.Xfer, first.Xfer) {
		t.Fatalf("its data: %+v %v", s, err)
	}
	if !strings.HasPrefix(first.Name, "screenshot-") || !strings.HasSuffix(first.Name, ".png") || strings.ContainsAny(first.Name, `/\`) {
		t.Errorf("its name: %q", first.Name)
	}
	if img, err := png.Decode(bytes.NewReader(got)); err != nil || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 {
		t.Fatalf("the picture: %v", err)
	}
	if asked[0] != 1 || asked[1] != ":1" {
		t.Errorf("asked for %v", asked)
	}
	if out := string(task.out); !strings.Contains(out, "screen 1 of 2 (HDMI-1): 64 × 48 pixels, a PNG of") {
		t.Errorf("its output: %q", out)
	}
}

// One that cannot be taken (no desktop, no display) fails at once, saying
// why, and sends nothing.
func TestScreenshotFails(t *testing.T) {
	swapShot(t, nil, errors.New("the minion runs in session 0, as a service: it has no desktop to take"))
	r := startShot(t, protocol.Args{})
	task := waitEnd(t, r, 2*time.Second)
	if task.ok || !strings.Contains(task.err, "session 0") || task.xfer != nil {
		t.Fatalf("ended %+v", task)
	}
	swapShot(t, &shot{img: testPicture(2, 2)}, nil)
	r = startShot(t, protocol.Args{screenshotFormat: "gif"})
	if task := waitEnd(t, r, 2*time.Second); task.ok || !strings.Contains(task.err, "png or jpeg") {
		t.Fatalf("a gif: %+v", task)
	}
}

// PNG by default; JPEG at the quality asked for, clamped to 1..100.
func TestEncodeShot(t *testing.T) {
	img := testPicture(40, 30)
	b, ext, err := encodeShot(img, "", 0)
	if err != nil || ext != "png" {
		t.Fatalf("png: %s %v", ext, err)
	}
	if back, err := png.Decode(bytes.NewReader(b)); err != nil || back.At(5, 7) != img.At(5, 7) {
		t.Fatalf("png back: %v", err)
	}
	low, ext, err := encodeShot(img, "jpeg", -5)
	if err != nil || ext != "jpg" {
		t.Fatalf("jpeg: %s %v", ext, err)
	}
	high, _, _ := encodeShot(img, "jpeg", 500)
	if _, err := jpeg.Decode(bytes.NewReader(low)); err != nil || len(low) >= len(high) {
		t.Fatalf("jpeg qualities: %d bytes at 1, %d at 100, %v", len(low), len(high), err)
	}
}

// Screen 1 is the host's main one, the rest in its order after it; the
// host's own list is left as it was.
func TestPickScreen(t *testing.T) {
	ms := []monitor{{name: "DP-1"}, {name: "HDMI-1", primary: true}, {name: "eDP-1"}}
	for n, want := range []string{"", "HDMI-1", "DP-1", "eDP-1"} {
		if n == 0 {
			continue
		}
		if m, err := pickScreen(ms, n); err != nil || m.name != want {
			t.Errorf("screen %d: %q %v, want %q", n, m.name, err, want)
		}
	}
	if ms[0].name != "DP-1" || ms[1].name != "HDMI-1" {
		t.Errorf("the host's list changed: %+v", ms)
	}
	if _, err := pickScreen(ms, 4); err == nil || !strings.Contains(err.Error(), "the host has 3") {
		t.Errorf("past them: %v", err)
	}
	if m, _ := pickScreen([]monitor{{name: "a"}, {name: "b"}}, 1); m.name != "a" {
		t.Errorf("no main one: %q", m.name)
	}
	if _, err := pickScreen(nil, 1); err == nil || !strings.Contains(err.Error(), "take screen 0") {
		t.Errorf("none: %v", err)
	}
}
