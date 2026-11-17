//go:build !windows

package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math/bits"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// An X display, as far as a screenshot needs one (X Window System Protocol,
// X11R7.7; the RandR extension, 1.5): the connection setup, with the
// user's MIT-MAGIC-COOKIE-1 when an Xauthority file has one; GetImage of the
// root window; RandR's monitors, for one screen of several. Plain Go over
// the display's socket: the minion takes no library, and no cgo, for it.

// xWait bounds a screenshot's talk with the X server.
const xWait = 15 * time.Second

// xMaxReply bounds a reply: a picture of 16384 × 16384 pixels.
const xMaxReply = 1 << 30

// xle: the connection's byte order, the client's to pick ('l').
var xle = binary.LittleEndian

// capture takes a picture of screen n (0: every screen) of X display d:
// DISPLAY's when d is empty, else :0.
func capture(n int, d string) (*shot, error) {
	if d == "" {
		d = os.Getenv("DISPLAY")
	}
	if d == "" {
		d = ":0"
	}
	x, err := dialX(d)
	if err != nil {
		return nil, fmt.Errorf("the X display %s: %w", d, err)
	}
	defer x.close()
	area := monitor{w: x.width, h: x.height}
	screens := 0
	if n > 0 {
		ms, err := x.monitors()
		if err != nil {
			return nil, fmt.Errorf("the X display %s: %w", d, err)
		}
		if area, err = pickScreen(ms, n); err != nil {
			return nil, err
		}
		screens = len(ms)
	}
	img, err := x.image(area.x, area.y, area.w, area.h)
	if err != nil {
		return nil, fmt.Errorf("the X display %s: %w", d, err)
	}
	return &shot{img: img, display: area.name, screens: screens}, nil
}

// xConn is a connection to an X display, at one of its screens.
type xConn struct {
	c   net.Conn
	r   *bufio.Reader
	seq uint16

	root          uint32
	width, height int
	// The root's pixels: bits each and a row's padding (ZPixmap), the
	// server's byte order, and the root visual's red, green and blue.
	bpp, pad int
	msb      bool
	masks    [3]uint32
}

// xAddress is where display d listens, its number and screen: ":1" a socket
// of this host (/tmp/.X11-unix/X1), "host:0" TCP port 6000, "/path:0" the
// socket of that path (XQuartz names its own so); ".1" after the number
// picks screen 1.
func xAddress(d string) (network, addr, number string, screen int, err error) {
	i := strings.LastIndexByte(d, ':')
	if i < 0 {
		return "", "", "", 0, fmt.Errorf("%q names no display (:0, host:0)", d)
	}
	host, number := d[:i], d[i+1:]
	if j := strings.IndexByte(number, '.'); j >= 0 {
		if screen, err = strconv.Atoi(number[j+1:]); err != nil || screen < 0 {
			return "", "", "", 0, fmt.Errorf("%q: a screen is a number", d)
		}
		number = number[:j]
	}
	n, err := strconv.Atoi(number)
	if err != nil || n < 0 || n > 65535-6000 {
		return "", "", "", 0, fmt.Errorf("%q: a display is a number", d)
	}
	switch {
	case strings.HasPrefix(host, "/"):
		return "unix", host + ":" + number, number, screen, nil
	case host == "" || host == "unix":
		return "unix", "/tmp/.X11-unix/X" + number, number, screen, nil
	}
	return "tcp", net.JoinHostPort(host, strconv.Itoa(6000+n)), number, screen, nil
}

// dialX connects to display d, at its screen.
func dialX(d string) (*xConn, error) {
	network, addr, number, screen, err := xAddress(d)
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout(network, addr, xWait)
	if err != nil && network == "unix" && strings.HasPrefix(addr, "/tmp/.X11-unix/") {
		// Linux's abstract socket of the same name, where the file is not
		// to be seen (another mount namespace).
		if ac, aerr := net.DialTimeout("unix", "@"+addr, xWait); aerr == nil {
			c, err = ac, nil
		}
	}
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(xWait))
	x := &xConn{c: c, r: bufio.NewReaderSize(c, 64<<10)}
	if err := x.setup(xCookie(number, network == "unix"), screen); err != nil {
		_ = c.Close()
		return nil, err
	}
	return x, nil
}

func (x *xConn) close() { _ = x.c.Close() }

// Xauthority families (Xau).
const (
	xFamilyInternet  = 0
	xFamilyInternet6 = 6
	xFamilyLocal     = 256
	xFamilyWild      = 65535
)

// xCookie is the MIT-MAGIC-COOKIE-1 for display number, of this host
// (local) or another, from XAUTHORITY's file, else ~/.Xauthority: none when
// there is no file or entry, and the server may let the minion in anyway
// (xhost).
func xCookie(number string, local bool) []byte {
	path := os.Getenv("XAUTHORITY")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		path = filepath.Join(home, ".Xauthority")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	host, _ := os.Hostname()
	return cookieIn(b, number, host, local)
}

// cookieIn is display number's cookie among an Xauthority file's entries:
// this host's by name before any other local one, then a wildcard's.
func cookieIn(b []byte, number, host string, local bool) []byte {
	var found []byte
	for len(b) >= 2 {
		family := binary.BigEndian.Uint16(b)
		b = b[2:]
		var field [4][]byte // address, display number, name, data
		for i := range field {
			if len(b) < 2 || len(b) < 2+int(binary.BigEndian.Uint16(b)) {
				return found
			}
			n := int(binary.BigEndian.Uint16(b))
			field[i], b = b[2:2+n], b[2+n:]
		}
		addr, num, name, data := field[0], field[1], field[2], field[3]
		if string(name) != "MIT-MAGIC-COOKIE-1" || (len(num) > 0 && string(num) != number) {
			continue
		}
		switch {
		case local && family == xFamilyLocal && string(addr) == host:
			return data
		case family == xFamilyWild, local && family == xFamilyLocal,
			!local && (family == xFamilyInternet || family == xFamilyInternet6):
			if found == nil {
				found = data
			}
		}
	}
	return found
}

// pad4 pads b to a multiple of 4 bytes.
func pad4(b []byte) []byte {
	return append(b, make([]byte, (4-len(b)%4)%4)...)
}

// setup opens the connection, with cookie when there is one, and reads
// screen n's root window and how its pixels are laid out.
func (x *xConn) setup(cookie []byte, n int) error {
	name := ""
	if cookie != nil {
		name = "MIT-MAGIC-COOKIE-1"
	}
	req := []byte{'l', 0}
	req = xle.AppendUint16(req, 11) // protocol 11.0
	req = xle.AppendUint16(req, 0)
	req = xle.AppendUint16(req, uint16(len(name)))
	req = xle.AppendUint16(req, uint16(len(cookie)))
	req = append(req, 0, 0)
	req = pad4(append(req, name...))
	req = pad4(append(req, cookie...))
	if _, err := x.c.Write(req); err != nil {
		return err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(x.r, head); err != nil {
		return fmt.Errorf("the X server's answer: %w", err)
	}
	body := make([]byte, int(xle.Uint16(head[6:]))*4)
	if _, err := io.ReadFull(x.r, body); err != nil {
		return fmt.Errorf("the X server's answer: %w", err)
	}
	switch head[0] {
	case 1:
		return x.readSetup(body, n)
	case 0:
		reason := body[:min(int(head[1]), len(body))]
		return fmt.Errorf("the X server refused the minion: %s", strings.TrimSpace(string(reason)))
	}
	return fmt.Errorf("the X server asks for more than a cookie: %s", strings.TrimRight(string(body), "\x00 \n"))
}

// xReader reads a reply's fields in order; short: one ran past its end.
type xReader struct {
	b     []byte
	off   int
	short bool
}

func (r *xReader) take(n int) []byte {
	if n < 0 || r.off+n > len(r.b) {
		r.short = true
		r.off = len(r.b)
		return make([]byte, max(n, 0))
	}
	b := r.b[r.off : r.off+n]
	r.off += n
	return b
}

func (r *xReader) skip(n int)   { r.take(n) }
func (r *xReader) u8() byte     { return r.take(1)[0] }
func (r *xReader) u16() uint16  { return xle.Uint16(r.take(2)) }
func (r *xReader) u32() uint32  { return xle.Uint32(r.take(4)) }
func (r *xReader) i16() int     { return int(int16(r.u16())) }
func (r *xReader) padded(n int) { r.skip((n + 3) / 4 * 4) }

// readSetup reads the setup's screen n: its root window, size, and the
// layout of its pixels (the root depth's format, the root visual's masks).
func (x *xConn) readSetup(b []byte, n int) error {
	r := &xReader{b: b}
	r.skip(16) // release, resource ids, motion buffer
	vendor := int(r.u16())
	r.skip(2) // maximum request length
	screens, formats := int(r.u8()), int(r.u8())
	x.msb = r.u8() == 1
	r.skip(9) // bitmap order, unit and pad, keycodes, unused
	r.padded(vendor)
	type format struct{ bpp, pad int }
	byDepth := map[byte]format{}
	for range formats {
		depth, bpp, pad := r.u8(), r.u8(), r.u8()
		r.skip(5)
		byDepth[depth] = format{int(bpp), int(pad)}
	}
	if n >= screens {
		return fmt.Errorf("the display has %d screens: no screen %d", screens, n)
	}
	for i := 0; i <= n && !r.short; i++ {
		root := r.u32()
		r.skip(16) // colormap, white and black pixels, input masks
		w, h := r.u16(), r.u16()
		r.skip(8) // millimetres, installed maps
		visual := r.u32()
		r.skip(2) // backing stores, save unders
		depth := r.u8()
		depths := int(r.u8())
		var masks [3]uint32
		seen := false
		for range depths {
			r.skip(2) // depth, unused
			visuals := int(r.u16())
			r.skip(4)
			for range visuals {
				id := r.u32()
				r.skip(4) // class, bits per value, colormap entries
				red, green, blue := r.u32(), r.u32(), r.u32()
				r.skip(4)
				if id == visual {
					masks, seen = [3]uint32{red, green, blue}, true
				}
			}
		}
		if i < n {
			continue
		}
		f, ok := byDepth[depth]
		if !seen || !ok {
			return fmt.Errorf("the X server gives no layout for the root window's pixels (depth %d)", depth)
		}
		x.root, x.width, x.height, x.bpp, x.pad, x.masks = root, int(w), int(h), f.bpp, f.pad, masks
	}
	if r.short {
		return errors.New("the X server's setup is cut short")
	}
	if x.bpp != 16 && x.bpp != 24 && x.bpp != 32 || x.pad < 8 || x.pad%8 != 0 {
		return fmt.Errorf("pixels of %d bits, rows padded to %d: not a layout this minion reads", x.bpp, x.pad)
	}
	return nil
}

// xErrors names the X errors a screenshot may meet.
var xErrors = map[byte]string{1: "BadRequest", 2: "BadValue", 3: "BadWindow", 5: "BadAtom", 8: "BadMatch",
	9: "BadDrawable", 10: "BadAccess", 11: "BadAlloc", 16: "BadLength", 17: "BadImplementation"}

// request sends a request and reads its reply: its 32-byte head and what
// follows. An X error is an error; events, which none was asked for, are
// passed over.
func (x *xConn) request(req []byte) ([]byte, error) {
	x.seq++
	if _, err := x.c.Write(req); err != nil {
		return nil, err
	}
	for {
		head := make([]byte, 32)
		if _, err := io.ReadFull(x.r, head); err != nil {
			return nil, err
		}
		switch head[0] &^ 0x80 {
		case 0:
			name := xErrors[head[1]]
			if name == "" {
				name = "error " + strconv.Itoa(int(head[1]))
			}
			return nil, fmt.Errorf("the X server answered %s", name)
		case 1:
			n := uint64(xle.Uint32(head[4:])) * 4
			if n > xMaxReply {
				return nil, fmt.Errorf("an X reply of %d bytes", n)
			}
			reply := make([]byte, 32+int(n))
			copy(reply, head)
			if _, err := io.ReadFull(x.r, reply[32:]); err != nil {
				return nil, err
			}
			if xle.Uint16(head[2:]) != x.seq {
				continue // an earlier request's: none waits for it
			}
			return reply, nil
		case 35:
			// A generic event: its length follows, as a reply's does.
			if _, err := io.CopyN(io.Discard, x.r, int64(xle.Uint32(head[4:]))*4); err != nil {
				return nil, err
			}
		}
	}
}

// image is a part of the root window, its pixels as RGBA.
func (x *xConn) image(rx, ry, w, h int) (*image.RGBA, error) {
	if w <= 0 || h <= 0 || w > 0xFFFF || h > 0xFFFF {
		return nil, fmt.Errorf("a picture of %d × %d pixels", w, h)
	}
	req := []byte{73, 2} // GetImage, ZPixmap
	req = xle.AppendUint16(req, 5)
	req = xle.AppendUint32(req, x.root)
	req = xle.AppendUint16(req, uint16(int16(rx)))
	req = xle.AppendUint16(req, uint16(int16(ry)))
	req = xle.AppendUint16(req, uint16(w))
	req = xle.AppendUint16(req, uint16(h))
	req = xle.AppendUint32(req, 0xFFFFFFFF) // every plane
	reply, err := x.request(req)
	if err != nil {
		return nil, fmt.Errorf("GetImage: %w", err)
	}
	return x.pixels(reply[32:], w, h)
}

// pixels is ZPixmap data in the root's layout as an RGBA picture.
func (x *xConn) pixels(data []byte, w, h int) (*image.RGBA, error) {
	size := x.bpp / 8
	stride := (w*x.bpp + x.pad - 1) / x.pad * x.pad / 8
	if len(data) < stride*h {
		return nil, fmt.Errorf("the X server sent %d bytes of pixels, not %d", len(data), stride*h)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fast := size == 4 && !x.msb && x.masks == [3]uint32{0xFF0000, 0xFF00, 0xFF}
	var ch [3]struct{ shift, width int }
	for i, m := range x.masks {
		ch[i].shift, ch[i].width = bits.TrailingZeros32(m), bits.OnesCount32(m)
	}
	for y := range h {
		row, out := data[y*stride:], img.Pix[y*img.Stride:]
		for i := range w {
			p, o := row[i*size:i*size+size], out[i*4:i*4+4]
			if fast { // BGRX, the common case
				o[0], o[1], o[2], o[3] = p[2], p[1], p[0], 255
				continue
			}
			var v uint32
			for k := range size {
				if x.msb {
					v = v<<8 | uint32(p[k])
				} else {
					v |= uint32(p[k]) << (8 * k)
				}
			}
			for c := range ch {
				o[c] = scale8(v>>ch[c].shift&(1<<ch[c].width-1), ch[c].width)
			}
			o[3] = 255
		}
	}
	return img, nil
}

// scale8 is a channel's value of width bits as 8 bits: 5 bits' 31 is 255.
func scale8(v uint32, width int) byte {
	switch {
	case width == 0:
		return 0
	case width >= 8:
		return byte(v >> (width - 8))
	}
	return byte(v * 255 / (1<<width - 1))
}

// monitors are the display's monitors, as RandR 1.5 names them; none when
// its server has no RandR 1.5.
func (x *xConn) monitors() ([]monitor, error) {
	const ext = "RANDR"
	req := []byte{98, 0} // QueryExtension
	req = xle.AppendUint16(req, uint16(2+(len(ext)+3)/4))
	req = xle.AppendUint16(req, uint16(len(ext)))
	req = append(req, 0, 0)
	reply, err := x.request(pad4(append(req, ext...)))
	if err != nil {
		return nil, fmt.Errorf("QueryExtension: %w", err)
	}
	if reply[8] == 0 {
		return nil, nil
	}
	op := reply[9]
	req = []byte{op, 0} // RRQueryVersion
	req = xle.AppendUint16(req, 3)
	req = xle.AppendUint32(req, 1)
	req = xle.AppendUint32(req, 5)
	if reply, err = x.request(req); err != nil {
		return nil, fmt.Errorf("RRQueryVersion: %w", err)
	}
	if major, minor := xle.Uint32(reply[8:]), xle.Uint32(reply[12:]); major < 1 || major == 1 && minor < 5 {
		return nil, nil
	}
	req = []byte{op, 42} // RRGetMonitors, the active ones
	req = xle.AppendUint16(req, 3)
	req = xle.AppendUint32(req, x.root)
	if reply, err = x.request(append(req, 1, 0, 0, 0)); err != nil {
		return nil, fmt.Errorf("RRGetMonitors: %w", err)
	}
	r := &xReader{b: reply, off: 12}
	count := int(r.u32())
	r.skip(16) // outputs in all, unused
	var ms []monitor
	var atoms []uint32
	for range count {
		atom := r.u32()
		primary := r.u8() != 0
		r.skip(1) // automatic
		outputs := int(r.u16())
		mx, my := r.i16(), r.i16()
		w, h := int(r.u16()), int(r.u16())
		r.skip(8 + 4*outputs) // millimetres, outputs
		if r.short {
			return nil, errors.New("RRGetMonitors: cut short")
		}
		ms = append(ms, monitor{x: mx, y: my, w: w, h: h, primary: primary})
		atoms = append(atoms, atom)
	}
	for i, atom := range atoms {
		if ms[i].name, err = x.atomName(atom); err != nil {
			return nil, fmt.Errorf("GetAtomName: %w", err)
		}
	}
	return ms, nil
}

// atomName is an atom's name: a monitor's (HDMI-1).
func (x *xConn) atomName(atom uint32) (string, error) {
	req := []byte{17, 0} // GetAtomName
	req = xle.AppendUint16(req, 2)
	reply, err := x.request(xle.AppendUint32(req, atom))
	if err != nil {
		return "", err
	}
	n := int(xle.Uint16(reply[8:]))
	if 32+n > len(reply) {
		return "", errors.New("cut short")
	}
	return string(reply[32 : 32+n]), nil
}
