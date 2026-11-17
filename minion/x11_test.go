//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"image/color"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestXAddress(t *testing.T) {
	for d, want := range map[string][3]string{
		":0":            {"unix", "/tmp/.X11-unix/X0", "0"},
		":1.2":          {"unix", "/tmp/.X11-unix/X1", "1"},
		"unix:3":        {"unix", "/tmp/.X11-unix/X3", "3"},
		"gw:2":          {"tcp", "gw:6002", "2"},
		"/tmp/x/sock:0": {"unix", "/tmp/x/sock:0", "0"},
	} {
		network, addr, number, _, err := xAddress(d)
		if err != nil || [3]string{network, addr, number} != want {
			t.Errorf("%s: %s %s %s %v", d, network, addr, number, err)
		}
	}
	if _, _, _, screen, _ := xAddress(":1.2"); screen != 2 {
		t.Errorf(":1.2: screen %d", screen)
	}
	for _, d := range []string{"nope", ":x", ":1.y", ":-1"} {
		if _, _, _, _, err := xAddress(d); err == nil {
			t.Errorf("%q passed", d)
		}
	}
}

// xauthEntry is an Xauthority file's entry for a MIT-MAGIC-COOKIE-1.
func xauthEntry(family uint16, addr, number string, cookie []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, family)
	for _, f := range [][]byte{[]byte(addr), []byte(number), []byte("MIT-MAGIC-COOKIE-1"), cookie} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(f)))
		b = append(b, f...)
	}
	return b
}

// A display's cookie: this host's own, before another local one or a
// wildcard; over TCP no local one; a file cut short reads as far as it
// goes.
func TestCookieIn(t *testing.T) {
	file := bytes.Join([][]byte{
		xauthEntry(xFamilyLocal, "otherhost", "0", []byte("A")),
		xauthEntry(xFamilyLocal, "thishost", "0", []byte("B")),
		xauthEntry(xFamilyWild, "", "", []byte("C")),
		xauthEntry(xFamilyLocal, "thishost", "1", []byte("D")),
	}, nil)
	for _, c := range []struct {
		number, host string
		local        bool
		want         string
	}{
		{"0", "thishost", true, "B"}, {"1", "thishost", true, "D"}, {"2", "thishost", true, "C"},
		{"0", "nohost", true, "A"}, {"0", "thishost", false, "C"},
	} {
		if got := cookieIn(file, c.number, c.host, c.local); string(got) != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
	// D, display 1's own, is cut off: the wildcard is what is left.
	if got := cookieIn(file[:len(file)-3], "1", "thishost", true); string(got) != "C" {
		t.Errorf("cut short: %q", got)
	}
}

// Pixels of another layout: 16 bits, RGB 565, the server's bytes most
// significant first, rows padded to 32 bits.
func TestXPixels565(t *testing.T) {
	x := &xConn{bpp: 16, pad: 32, msb: true, masks: [3]uint32{0xF800, 0x07E0, 0x001F}}
	img, err := x.pixels([]byte{0xF8, 0x00, 0x07, 0xE0, 0xFF, 0xFF, 0, 0}, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {255, 255, 255, 255}} {
		if got := img.RGBAAt(i, 0); got != want {
			t.Errorf("pixel %d: %v, want %v", i, got, want)
		}
	}
	if _, err := x.pixels(make([]byte, 7), 3, 1); err == nil {
		t.Error("a row cut short passed")
	}
}

// fakeX is an X server for the test: one screen of w × h pixels, depth 24
// in 32 bits (BGRX); the cookie it lets in (none: any); RandR 1.5, with
// monitors, when randr. Before a picture it sends an event no one asked
// for, which the minion passes over.
type fakeX struct {
	w, h     int
	cookie   []byte
	randr    bool
	monitors []monitor
}

func fakePixel(x, y int) [3]byte { return [3]byte{byte(10 * x), byte(20 * y), byte(x + y)} }

func (f *fakeX) serve(t *testing.T, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go f.conn(t, c)
	}
}

func (f *fakeX) conn(t *testing.T, c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		return
	}
	pad := func(n int) int { return (n + 3) / 4 * 4 }
	name := make([]byte, pad(int(xle.Uint16(head[6:]))))
	data := make([]byte, pad(int(xle.Uint16(head[8:]))))
	_, _ = io.ReadFull(r, name)
	_, _ = io.ReadFull(r, data)
	name, data = name[:xle.Uint16(head[6:])], data[:xle.Uint16(head[8:])]
	if f.cookie != nil && (string(name) != "MIT-MAGIC-COOKIE-1" || !bytes.Equal(data, f.cookie)) {
		reason := "No protocol specified\n"
		b := []byte{0, byte(len(reason)), 11, 0, 0, 0}
		b = xle.AppendUint16(b, uint16(pad(len(reason))/4))
		_, _ = c.Write(pad4(append(b, reason...)))
		return
	}
	body := make([]byte, 16)
	body = xle.AppendUint16(body, 4)      // vendor
	body = xle.AppendUint16(body, 0xFFFF) // maximum request length
	body = append(body, 1, 2, 0, 0, 32, 32, 8, 255, 0, 0, 0, 0)
	body = append(body, "fake"...)
	body = append(body, 1, 1, 32, 0, 0, 0, 0, 0, 24, 32, 32, 0, 0, 0, 0, 0) // formats
	body = xle.AppendUint32(body, 0x123)                                    // root
	body = append(body, make([]byte, 16)...)
	body = xle.AppendUint16(body, uint16(f.w))
	body = xle.AppendUint16(body, uint16(f.h))
	body = append(body, make([]byte, 8)...)
	body = xle.AppendUint32(body, 0x21) // root visual
	body = append(body, 0, 0, 24, 1)    // backing, save unders, depth 24, one depth
	body = append(body, 24, 0, 1, 0, 0, 0, 0, 0)
	body = xle.AppendUint32(body, 0x21)
	body = append(body, 4, 8, 0, 1)
	body = xle.AppendUint32(body, 0xFF0000)
	body = xle.AppendUint32(body, 0xFF00)
	body = xle.AppendUint32(body, 0xFF)
	body = append(body, 0, 0, 0, 0)
	reply := xle.AppendUint16([]byte{1, 0, 11, 0, 0, 0}, uint16(len(body)/4))
	if _, err := c.Write(append(reply, body...)); err != nil {
		return
	}
	var seq uint16
	answer := func(head []byte, extra []byte) {
		h := make([]byte, 32)
		copy(h, head)
		h[0] = 1
		xle.PutUint16(h[2:], seq)
		xle.PutUint32(h[4:], uint32(len(extra)/4))
		_, _ = c.Write(append(h, extra...))
	}
	for {
		req := make([]byte, 4)
		if _, err := io.ReadFull(r, req); err != nil {
			return
		}
		rest := make([]byte, int(xle.Uint16(req[2:]))*4-4)
		if _, err := io.ReadFull(r, rest); err != nil {
			return
		}
		seq++
		switch req[0] {
		case 73: // GetImage
			x, y := int(int16(xle.Uint16(rest[4:]))), int(int16(xle.Uint16(rest[6:])))
			w, h := int(xle.Uint16(rest[8:])), int(xle.Uint16(rest[10:]))
			if x < 0 || y < 0 || x+w > f.w || y+h > f.h {
				e := make([]byte, 32)
				e[1] = 8 // BadMatch
				xle.PutUint16(e[2:], seq)
				_, _ = c.Write(e)
				continue
			}
			_, _ = c.Write(append([]byte{12}, make([]byte, 31)...)) // Expose
			var px []byte
			for j := range h {
				for i := range w {
					p := fakePixel(x+i, y+j)
					px = append(px, p[2], p[1], p[0], 0)
				}
			}
			answer([]byte{1, 24}, px)
		case 98: // QueryExtension
			n := int(xle.Uint16(rest[0:]))
			h := make([]byte, 10)
			if string(rest[4:4+n]) == "RANDR" && f.randr {
				h[8], h[9] = 1, 140
			}
			answer(h, nil)
		case 140: // RandR
			switch req[1] {
			case 0: // QueryVersion
				h := xle.AppendUint32(xle.AppendUint32(make([]byte, 8), 1), 5)
				answer(h, nil)
			case 42: // GetMonitors
				h := xle.AppendUint32(xle.AppendUint32(make([]byte, 12), uint32(len(f.monitors))), uint32(len(f.monitors)))
				var ms []byte
				for i, m := range f.monitors {
					ms = xle.AppendUint32(ms, uint32(300+i))
					primary := byte(0)
					if m.primary {
						primary = 1
					}
					ms = append(ms, primary, 1)
					ms = xle.AppendUint16(ms, 1)
					for _, v := range []int{m.x, m.y, m.w, m.h} {
						ms = xle.AppendUint16(ms, uint16(v))
					}
					ms = xle.AppendUint32(xle.AppendUint32(ms, 300), 200) // millimetres
					ms = xle.AppendUint32(ms, uint32(0x60+i))             // its output
				}
				answer(h, ms)
			}
		case 17: // GetAtomName
			name := f.monitors[xle.Uint32(rest[0:])-300].name
			answer(xle.AppendUint16(make([]byte, 8), uint16(len(name))), pad4([]byte(name)))
		}
	}
}

// startFakeX serves f at a socket of the test's own, named as a display:
// "<dir>/x:0".
func startFakeX(t *testing.T, f *fakeX) string {
	t.Helper()
	display := filepath.Join(t.TempDir(), "x:0")
	l, err := net.Listen("unix", display)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go f.serve(t, l)
	return display
}

// A picture of an X display: the whole of its screen, or one monitor's
// part, the main one first; let in by this host's cookie, refused another.
func TestXCapture(t *testing.T) {
	f := &fakeX{w: 6, h: 4, cookie: []byte("0123456789abcdef"), randr: true,
		monitors: []monitor{{name: "DP-1", x: 0, y: 0, w: 3, h: 4}, {name: "HDMI-1", x: 3, y: 0, w: 3, h: 4, primary: true}}}
	display := startFakeX(t, f)
	host, _ := os.Hostname()
	auth := filepath.Join(t.TempDir(), "Xauthority")
	if err := os.WriteFile(auth, xauthEntry(xFamilyLocal, host, "0", f.cookie), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XAUTHORITY", auth)

	pixelsOf := func(s *shot, x0 int) bool {
		for y := range s.img.Bounds().Dy() {
			for x := range s.img.Bounds().Dx() {
				p, c := fakePixel(x0+x, y), s.img.RGBAAt(x, y)
				if c != (color.RGBA{p[0], p[1], p[2], 255}) {
					return false
				}
			}
		}
		return true
	}
	s, err := capture(0, display)
	if err != nil || s.img.Bounds().Dx() != 6 || s.img.Bounds().Dy() != 4 || s.display != "" || !pixelsOf(s, 0) {
		t.Fatalf("every screen: %+v %v", s, err)
	}
	s, err = capture(1, display)
	if err != nil || s.img.Bounds().Dx() != 3 || s.display != "HDMI-1" || s.screens != 2 || !pixelsOf(s, 3) {
		t.Fatalf("the main screen: %+v %v", s, err)
	}
	if s, err = capture(2, display); err != nil || s.display != "DP-1" || !pixelsOf(s, 0) {
		t.Fatalf("the second: %+v %v", s, err)
	}
	if _, err := capture(3, display); err == nil || !strings.Contains(err.Error(), "the host has 2") {
		t.Fatalf("a third: %v", err)
	}

	f.randr = false
	if _, err := capture(1, display); err == nil || !strings.Contains(err.Error(), "take screen 0") {
		t.Fatalf("no RandR: %v", err)
	}
	if err := os.WriteFile(auth, xauthEntry(xFamilyLocal, host, "0", []byte("another cookie!!")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(0, display); err == nil || !strings.Contains(err.Error(), "refused the minion: No protocol specified") {
		t.Fatalf("another cookie: %v", err)
	}
	if _, err := capture(0, filepath.Join(t.TempDir(), "none:0")); err == nil || !strings.Contains(err.Error(), "X display") {
		t.Fatalf("no display: %v", err)
	}
}

// The same against a real X server, where the host has Xvfb: a display of
// its own (-displayfd picks a free one), let in by a cookie (-auth), white
// (-wr). Never the user's display.
func TestXvfb(t *testing.T) {
	bin, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skip("no Xvfb on this host")
	}
	dir := t.TempDir()
	cookie := bytes.Repeat([]byte{0x5a}, 16)
	auth := filepath.Join(dir, "Xauthority")
	if err := os.WriteFile(auth, xauthEntry(xFamilyWild, "", "", cookie), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-displayfd", "3", "-auth", auth, "-nolisten", "tcp", "-noreset", "-wr", "-screen", "0", "320x200x24")
	cmd.ExtraFiles = []*os.File{wr}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = wr.Close()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	number := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(rd).ReadString('\n')
		number <- strings.TrimSpace(line)
	}()
	var display string
	select {
	case n := <-number:
		display = ":" + n
	case <-time.After(10 * time.Second):
		t.Fatal("Xvfb did not start")
	}
	t.Setenv("XAUTHORITY", auth)

	s, err := capture(0, display)
	if err != nil || s.img.Bounds().Dx() != 320 || s.img.Bounds().Dy() != 200 || s.img.RGBAAt(17, 9) != (color.RGBA{255, 255, 255, 255}) {
		t.Fatalf("every screen of %s: %v", display, err)
	}
	if s, err := capture(1, display); err != nil || s.img.Bounds().Dx() != 320 || s.display == "" || s.screens != 1 {
		t.Fatalf("its one monitor: %+v %v", s, err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, xauthEntry(xFamilyWild, "", "", bytes.Repeat([]byte{1}, 16)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XAUTHORITY", other)
	if _, err := capture(0, display); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("another cookie: %v", err)
	}
}
