package protocol

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// MaxIconSize is the largest module icon, in bytes.
const MaxIconSize = 16 << 10

// svgNamespace is the namespace an icon's root element must declare: without
// it browsers do not render an SVG image.
const svgNamespace = "http://www.w3.org/2000/svg"

// iconElements are the SVG elements an icon may use: shapes, structure,
// paint servers, clipping and text. Nothing that runs code, embeds other
// content or loads anything.
var iconElements = map[string]bool{
	"svg": true, "g": true, "defs": true, "symbol": true, "use": true, "title": true, "desc": true,
	"path": true, "rect": true, "circle": true, "ellipse": true, "line": true, "polyline": true, "polygon": true,
	"text": true, "tspan": true,
	"linearGradient": true, "radialGradient": true, "stop": true, "clipPath": true, "mask": true,
}

// iconURL finds url(...) references in attribute values; only local ones
// (url(#id)) are allowed.
var iconURL = regexp.MustCompile(`(?i)url\(\s*['"]?([^'")\s]*)`)

// ValidateIcon checks a module's SVG icon: at most MaxIconSize bytes of
// well-formed XML with one root svg element in the SVG namespace, built only
// from shapes, structure, paint servers, clipping and text. Event handler
// attributes, references other than to the icon's own elements (href="#id",
// url(#id)), DOCTYPEs and processing instructions other than the XML
// declaration are refused.
//
// The UI shows an icon as a single-colour glyph in its own text colour, like
// its built-in icons, so its colours do not matter: draw shapes on a
// transparent background, for example with stroke="currentColor". That
// colour is light on dark themes and dark on light ones. Line art reads the
// same either way; an icon whose shape is the lit parts of a shaded drawing
// reads as a negative on light themes. Such a module also registers an
// icon_light, drawn with the shadows and lines as the shape, which the UI
// shows on light themes instead; it is checked the same way.
func ValidateIcon(svg string) error {
	if strings.TrimSpace(svg) == "" {
		return errors.New("icon is empty")
	}
	if len(svg) > MaxIconSize {
		return fmt.Errorf("icon is larger than %d bytes", MaxIconSize)
	}
	dec := xml.NewDecoder(strings.NewReader(svg))
	depth, roots := 0, 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("icon is not well-formed XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return errors.New("icon has more than one root element")
				}
				if t.Name.Local != "svg" || t.Name.Space != svgNamespace {
					return fmt.Errorf(`icon's root element must be <svg xmlns=%q>`, svgNamespace)
				}
			}
			if t.Name.Space != svgNamespace || !iconElements[t.Name.Local] {
				return fmt.Errorf("icon element <%s> is not allowed", t.Name.Local)
			}
			if err := checkIconAttrs(t); err != nil {
				return err
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) > 0 {
				return errors.New("icon has text outside its root element")
			}
		case xml.ProcInst:
			if t.Target != "xml" {
				return fmt.Errorf("icon processing instruction <?%s?> is not allowed", t.Target)
			}
		case xml.Directive:
			return errors.New("icon DOCTYPE and other directives are not allowed")
		}
	}
	if roots == 0 {
		return errors.New("icon has no root element")
	}
	return nil
}

func checkIconAttrs(el xml.StartElement) error {
	for _, a := range el.Attr {
		name := strings.ToLower(a.Name.Local)
		if strings.HasPrefix(name, "on") {
			return fmt.Errorf("icon attribute %s on <%s> is not allowed", a.Name.Local, el.Name.Local)
		}
		if name == "href" && !strings.HasPrefix(strings.TrimSpace(a.Value), "#") {
			return fmt.Errorf("icon <%s> may only refer to its own elements (href=\"#id\")", el.Name.Local)
		}
		for _, m := range iconURL.FindAllStringSubmatch(a.Value, -1) {
			if !strings.HasPrefix(m[1], "#") {
				return fmt.Errorf("icon <%s> may only refer to its own elements (url(#id))", el.Name.Local)
			}
		}
	}
	return nil
}
