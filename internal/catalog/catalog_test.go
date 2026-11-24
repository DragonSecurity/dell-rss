package catalog

import (
	"bytes"
	"compress/gzip"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/unicode"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/catalog.xml")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// utf16 re-encodes the fixture the way Dell actually ships it.
func utf16(t *testing.T, data []byte, order unicode.Endianness, bom unicode.BOMPolicy) []byte {
	t.Helper()
	data = bytes.Replace(data, []byte(`encoding="utf-8"`), []byte(`encoding="utf-16"`), 1)
	out, err := unicode.UTF16(order, bom).NewEncoder().Bytes(data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseEncodings(t *testing.T) {
	raw := fixture(t)
	cases := map[string][]byte{
		"utf-8":              raw,
		"utf-8 bom":          append([]byte{0xEF, 0xBB, 0xBF}, raw...),
		"utf-16le bom":       utf16(t, raw, unicode.LittleEndian, unicode.UseBOM),
		"utf-16be bom":       utf16(t, raw, unicode.BigEndian, unicode.UseBOM),
		"utf-16le no bom":    utf16(t, raw, unicode.LittleEndian, unicode.IgnoreBOM),
		"gzip utf-16le bom":  gz(t, utf16(t, raw, unicode.LittleEndian, unicode.UseBOM)),
		"gzip utf-8":         gz(t, raw),
		"gzip utf-16be bom2": gz(t, utf16(t, raw, unicode.BigEndian, unicode.UseBOM)),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := len(c.Components); got != 5 {
				t.Fatalf("components = %d, want 5 (bundle contents must be skipped)", got)
			}
			if c.Manifest.Version != "26.11.23" || c.Manifest.ReleaseID != "VTXX2" {
				t.Errorf("manifest = %+v", c.Manifest)
			}
		})
	}
}

func TestParseComponent(t *testing.T) {
	c, err := Parse(bytes.NewReader(fixture(t)))
	if err != nil {
		t.Fatal(err)
	}

	bios := c.Components[0]
	want := Component{
		Path:            "FOLDER15148387M/1/BIOS_R3PGY_WN64_2.29.3.EXE",
		ReleaseID:       "R3PGY",
		Version:         "2.29.3",
		Posted:          time.Date(2026, 10, 20, 8, 17, 5, 0, time.UTC),
		Released:        time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		Size:            50905912,
		RebootRequired:  true,
		Name:            "Dell Server PowerEdge BIOS R740/R640 Version 2.29.3",
		Description:     "BIOS update & fixes.",
		RevisionHistory: "Fixes:\n- Security fixes.",
		Category:        "BIOS",
		ComponentType:   "BIOS",
		Criticality:     "Urgent",
		InfoURL:         "https://www.dell.com/support/home/us/en/19/Drivers/DriversDetails?driverId=R3PGY",
	}
	systems := bios.Systems
	bios.Systems = nil
	if bios.Posted != want.Posted || bios.Released != want.Released {
		t.Errorf("dates = %v / %v, want %v / %v", bios.Posted, bios.Released, want.Posted, want.Released)
	}
	bios.Posted, bios.Released, want.Posted, want.Released = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(bios, want) {
		t.Errorf("component =\n%+v\nwant\n%+v", bios, want)
	}

	if len(systems) != 2 || systems[0] != (System{Brand: "PowerEdge", Model: "R640", SystemID: "0716"}) {
		t.Errorf("systems = %+v", systems)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for name, input := range map[string]string{
		"not xml":    "hello world, this is not a catalog",
		"wrong root": `<?xml version="1.0"?><Other><SoftwareComponent/></Other>`,
		"truncated":  `<?xml version="1.0"?><Manifest><SoftwareComponent path="x">`,
		"too short":  "<",
		"bad gzip":   "\x1f\x8b\x00\x00garbage",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(input)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	cases := map[string]time.Time{
		"2026-10-20T03:17:05-05:00": time.Date(2026, 10, 20, 8, 17, 5, 0, time.UTC),
		"2026-10-20T03:17:05":       time.Date(2026, 10, 20, 3, 17, 5, 0, time.UTC),
		"2026-10-20":                time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		"October 20, 2026":          time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		"10/20/2026":                time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		" 1/2/2026 ":                time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		"":                          {},
		"soon":                      {},
	}
	for in, want := range cases {
		if got := ParseDate(in); !got.Equal(want) {
			t.Errorf("ParseDate(%q) = %v, want %v", in, got, want)
		}
	}
}
