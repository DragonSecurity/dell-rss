// Package catalog downloads and parses Dell's enterprise update catalog
// (https://downloads.dell.com/catalog/Catalog.xml.gz).
package catalog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Manifest describes the catalog release itself.
type Manifest struct {
	BaseLocation string
	Version      string
	ReleaseID    string
	Identifier   string
	Date         time.Time
}

// Catalog is the subset of the Dell catalog this service cares about.
type Catalog struct {
	Manifest   Manifest
	Components []Component
}

// Component is one downloadable update package.
type Component struct {
	Path            string
	ReleaseID       string
	Version         string
	Posted          time.Time // dateTime attribute: when Dell published this package to the catalog
	Released        time.Time // releaseDate attribute
	Size            int64
	RebootRequired  bool
	Name            string
	Description     string
	RevisionHistory string
	Category        string
	ComponentType   string // FRMW, DRVR, BIOS, APAC
	Criticality     string // Urgent, Recommended, Optional
	InfoURL         string
	Systems         []System
}

// System is one supported server model.
type System struct {
	Brand    string
	Model    string
	SystemID string
}

type rawDisplay struct {
	Display string `xml:"Display"`
}

type rawValueDisplay struct {
	Value   string `xml:"value,attr"`
	Display string `xml:"Display"`
}

type rawComponent struct {
	Path            string          `xml:"path,attr"`
	ReleaseID       string          `xml:"releaseID,attr"`
	VendorVersion   string          `xml:"vendorVersion,attr"`
	ReleaseDate     string          `xml:"releaseDate,attr"`
	DateTime        string          `xml:"dateTime,attr"`
	Size            string          `xml:"size,attr"`
	RebootRequired  string          `xml:"rebootRequired,attr"`
	Name            rawDisplay      `xml:"Name"`
	Description     rawDisplay      `xml:"Description"`
	RevisionHistory rawDisplay      `xml:"RevisionHistory"`
	Category        rawValueDisplay `xml:"Category"`
	ComponentType   rawValueDisplay `xml:"ComponentType"`
	Criticality     rawValueDisplay `xml:"Criticality"`
	ImportantInfo   struct {
		URL string `xml:"URL,attr"`
	} `xml:"ImportantInfo"`
	Brands []struct {
		Display string `xml:"Display"`
		Models  []struct {
			SystemID string `xml:"systemID,attr"`
			Display  string `xml:"Display"`
		} `xml:"Model"`
	} `xml:"SupportedSystems>Brand"`
}

// Parse reads a catalog that may be gzip-compressed and may be encoded as
// UTF-8 or UTF-16 (Dell currently ships UTF-16 LE with a BOM).
func Parse(r io.Reader) (*Catalog, error) {
	br := bufio.NewReader(r)

	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("open gzip: %w", err)
		}
		defer gz.Close()
		br = bufio.NewReader(gz)
	}

	reader, err := utf8Reader(br)
	if err != nil {
		return nil, err
	}

	return decode(reader)
}

// utf8Reader converts the stream to UTF-8 based on its byte order mark, or
// on the shape of the first character when there is no BOM.
func utf8Reader(br *bufio.Reader) (io.Reader, error) {
	header, err := br.Peek(4)
	if err != nil {
		return nil, fmt.Errorf("read catalog header: %w", err)
	}

	switch {
	case bytes.HasPrefix(header, []byte{0xFF, 0xFE}):
		return transform.NewReader(br, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()), nil
	case bytes.HasPrefix(header, []byte{0xFE, 0xFF}):
		return transform.NewReader(br, unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder()), nil
	case header[0] == '<' && header[1] == 0:
		return transform.NewReader(br, unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()), nil
	case header[0] == 0 && header[1] == '<':
		return transform.NewReader(br, unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder()), nil
	case bytes.HasPrefix(header, []byte{0xEF, 0xBB, 0xBF}):
		_, _ = br.Discard(3)
	}

	return br, nil
}

func decode(r io.Reader) (*Catalog, error) {
	decoder := xml.NewDecoder(r)

	// The stream is already UTF-8; the declaration may still say utf-16.
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	var (
		catalog     Catalog
		sawManifest bool
	)

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode catalog: %w", err)
		}

		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}

		switch start.Name.Local {
		case "Manifest":
			sawManifest = true
			catalog.Manifest = parseManifest(start)

		case "SoftwareComponent":
			var raw rawComponent
			if err := decoder.DecodeElement(&raw, &start); err != nil {
				return nil, fmt.Errorf("decode software component: %w", err)
			}
			catalog.Components = append(catalog.Components, convert(raw))

		default:
			// Bundles, inventory collectors, release notes: not needed.
			if sawManifest {
				if err := decoder.Skip(); err != nil {
					return nil, fmt.Errorf("decode catalog: %w", err)
				}
			}
		}
	}

	if !sawManifest {
		return nil, fmt.Errorf("decode catalog: no <Manifest> root element")
	}

	return &catalog, nil
}

func parseManifest(start xml.StartElement) Manifest {
	var m Manifest
	for _, attr := range start.Attr {
		switch attr.Name.Local {
		case "baseLocation":
			m.BaseLocation = attr.Value
		case "version":
			m.Version = attr.Value
		case "releaseID":
			m.ReleaseID = attr.Value
		case "identifier":
			m.Identifier = attr.Value
		case "dateTime":
			m.Date = ParseDate(attr.Value)
		}
	}
	return m
}

func convert(raw rawComponent) Component {
	c := Component{
		Path:            strings.TrimSpace(raw.Path),
		ReleaseID:       strings.TrimSpace(raw.ReleaseID),
		Version:         strings.TrimSpace(raw.VendorVersion),
		Posted:          ParseDate(raw.DateTime),
		Released:        ParseDate(raw.ReleaseDate),
		RebootRequired:  strings.EqualFold(raw.RebootRequired, "true"),
		Name:            strings.TrimSpace(raw.Name.Display),
		Description:     strings.TrimSpace(raw.Description.Display),
		RevisionHistory: strings.TrimSpace(raw.RevisionHistory.Display),
		Category:        strings.TrimSpace(raw.Category.Display),
		ComponentType:   strings.TrimSpace(raw.ComponentType.Value),
		Criticality:     strings.TrimSpace(raw.Criticality.Display),
		InfoURL:         strings.TrimSpace(raw.ImportantInfo.URL),
	}

	if size, err := strconv.ParseInt(raw.Size, 10, 64); err == nil {
		c.Size = size
	}

	for _, brand := range raw.Brands {
		for _, model := range brand.Models {
			c.Systems = append(c.Systems, System{
				Brand:    strings.TrimSpace(brand.Display),
				Model:    strings.TrimSpace(model.Display),
				SystemID: strings.TrimSpace(model.SystemID),
			})
		}
	}

	return c
}

var dateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"January 2, 2006",
	"01/02/2006 15:04:05",
	"01/02/2006",
	"1/2/2006",
}

// ParseDate parses the date formats seen in the Dell catalog. It returns the
// zero time when the value is empty or unrecognised.
func ParseDate(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range dateLayouts {
		if date, err := time.Parse(layout, value); err == nil {
			return date.UTC()
		}
	}
	return time.Time{}
}
