package feed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dragonsecurity/dell-rss/internal/catalog"
)

func snapshot(t *testing.T) *Snapshot {
	t.Helper()
	data, err := os.ReadFile("../catalog/testdata/catalog.xml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	info := catalog.Info{SHA256: "abc", LastModified: "Mon, 23 Nov 2026 09:46:27 GMT", FetchedAt: time.Now()}
	s, err := Build(c, info, DefaultBrands, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func slugs(models []*Model) []string {
	var out []string
	for _, m := range models {
		out = append(out, m.Slug)
	}
	return out
}

func TestDiscoversModels(t *testing.T) {
	s := snapshot(t)

	// Precision is not a server brand; "R750 xa" and "R750XA" are one model;
	// R730 comes from the "Enterprise Servers" brand.
	if got, want := slugs(s.Models), []string{"r640", "r730", "r740", "r750xa"}; !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	if s.Components != 3 {
		t.Errorf("components = %d, want 3 (duplicate and workstation-only dropped)", s.Components)
	}

	m, ok := s.Model("R640")
	if !ok {
		t.Fatal("R640 not found")
	}
	if len(m.Updates) != 3 {
		t.Fatalf("R640 updates = %d, want 3", len(m.Updates))
	}
	for i := 1; i < len(m.Updates); i++ {
		if m.Updates[i].Posted.After(m.Updates[i-1].Posted) {
			t.Fatal("updates are not newest first")
		}
	}
	if _, ok := s.Model("r750 XA"); !ok {
		t.Error("lookup should ignore case and spacing")
	}
	if _, ok := s.Model("t7920"); ok {
		t.Error("workstation model should not be indexed")
	}
}

func TestBrandsAreConfigurable(t *testing.T) {
	data, _ := os.ReadFile("../catalog/testdata/catalog.xml")
	c, _ := catalog.Parse(bytes.NewReader(data))
	s, err := Build(c, catalog.Info{}, []string{"precision"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := slugs(s.Models); !slices.Equal(got, []string{"t7920"}) {
		t.Fatalf("models = %v", got)
	}
	if _, err := Build(c, catalog.Info{}, []string{"Nonexistent"}, "test"); err == nil {
		t.Fatal("expected an error when no models match")
	}
}

// The GUID must match what the original single-model service emitted, or
// every subscriber sees every update again after upgrading.
func TestGUIDIsBackwardCompatible(t *testing.T) {
	m, _ := snapshot(t).Model("r640")
	link := "https://downloads.dell.com/FOLDER15148387M/1/BIOS_R3PGY_WN64_2.29.3.EXE"
	sum := sha256.Sum256([]byte(link + "|" + "2.29.3"))
	want := hex.EncodeToString(sum[:])

	for _, u := range m.Updates {
		if u.DownloadURL == link {
			if u.ID != want {
				t.Fatalf("GUID = %s, want %s", u.ID, want)
			}
			return
		}
	}
	t.Fatal("BIOS update not found")
}

func TestFilters(t *testing.T) {
	s := snapshot(t)
	m, _ := s.Model("r640")

	cases := map[string]int{
		"urgent":       1,
		"URGENT":       1,
		"recommended":  1,
		"optional":     1,
		"bios":         1,
		"idrac":        1,
		"network-rdma": 1,
	}
	for name, want := range cases {
		f, ok := s.Filter(name)
		if !ok {
			t.Errorf("filter %q not recognised", name)
			continue
		}
		if got := len(f.Apply(m.Updates)); got != want {
			t.Errorf("filter %q matched %d, want %d", name, got, want)
		}
	}

	if _, ok := s.Filter("nope"); ok {
		t.Error("unknown filter accepted")
	}

	// Known but empty for this model: valid, and yields an empty feed.
	r740, _ := s.Model("r740")
	idrac, _ := s.Filter("idrac")
	if got := idrac.Apply(r740.Updates); len(got) != 0 {
		t.Errorf("R740 iDRAC = %d", len(got))
	}

	var listed []string
	for _, fl := range s.Filters(m) {
		listed = append(listed, fl.Slug)
	}
	if want := []string{"urgent", "recommended", "optional", "bios", "idrac", "network-rdma"}; !slices.Equal(listed, want) {
		t.Errorf("Filters = %v, want %v", listed, want)
	}
}

func TestUnsafeInfoURLDropped(t *testing.T) {
	m, _ := snapshot(t).Model("r640")
	for _, u := range m.Updates {
		if strings.HasPrefix(u.InfoURL, "javascript:") {
			t.Fatalf("javascript: URL kept on %q", u.Name)
		}
	}
}

func TestRSS(t *testing.T) {
	s := snapshot(t)
	m, _ := s.Model("r640")
	bios, _ := s.Filter("bios")

	data, err := s.RSS(m, bios, "https://feeds.example/r640/bios/rss.xml")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Channel struct {
			Title         string `xml:"title"`
			LastBuildDate string `xml:"lastBuildDate"`
			Self          struct {
				Href string `xml:"href,attr"`
			} `xml:"http://www.w3.org/2005/Atom link"`
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				Description string `xml:"description"`
				PubDate     string `xml:"pubDate"`
				GUID        string `xml:"guid"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("feed is not valid XML: %v\n%s", err, data)
	}
	ch := doc.Channel
	if ch.Title != "Dell PowerEdge R640 BIOS Updates" {
		t.Errorf("title = %q", ch.Title)
	}
	if ch.LastBuildDate != "Mon, 23 Nov 2026 09:46:27 GMT" {
		t.Errorf("lastBuildDate = %q (must be the catalog date so output is deterministic)", ch.LastBuildDate)
	}
	if ch.Self.Href != "https://feeds.example/r640/bios/rss.xml" {
		t.Errorf("atom:link = %q", ch.Self.Href)
	}
	if len(ch.Items) != 1 {
		t.Fatalf("items = %d", len(ch.Items))
	}
	it := ch.Items[0]
	if it.PubDate != "Tue, 20 Oct 2026 08:17:05 GMT" {
		t.Errorf("pubDate = %q", it.PubDate)
	}
	if !strings.Contains(it.Link, "driverId=R3PGY") {
		t.Errorf("link = %q, want the Dell details page", it.Link)
	}
	if !strings.Contains(it.Description, "BIOS update &amp; fixes.") {
		t.Errorf("description not HTML-escaped: %q", it.Description)
	}

	again, _ := s.RSS(m, bios, "https://feeds.example/r640/bios/rss.xml")
	if !bytes.Equal(data, again) {
		t.Error("rendering is not deterministic")
	}
}

func TestRSSEscapesMarkup(t *testing.T) {
	s := snapshot(t)
	m, _ := s.Model("r640")
	data, err := s.RSS(m, All, "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("<script>")) {
		t.Fatal("raw <script> in feed")
	}
	if bytes.Contains(data, []byte("atom:link")) {
		t.Error("atom:link rendered without a self URL")
	}
}

func TestJSON(t *testing.T) {
	s := snapshot(t)

	var models struct {
		Default string `json:"default"`
		Models  []struct {
			Slug    string `json:"slug"`
			Updates int    `json:"updates"`
			RSS     string `json:"rss"`
		} `json:"models"`
	}
	data, err := s.ModelsJSON("r640")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &models); err != nil {
		t.Fatal(err)
	}
	if models.Default != "r640" || len(models.Models) != 4 || models.Models[0].RSS != "/r640/rss.xml" || models.Models[0].Updates != 3 {
		t.Errorf("models = %+v", models)
	}

	m, _ := s.Model("r740")
	urgent, _ := s.Filter("urgent")
	var one struct {
		Model   string    `json:"model"`
		Filter  string    `json:"filter"`
		Count   int       `json:"count"`
		Updates []*Update `json:"updates"`
	}
	data, _ = s.ModelJSON(m, urgent)
	if err := json.Unmarshal(data, &one); err != nil {
		t.Fatal(err)
	}
	if one.Model != "r740" || one.Filter != "urgent" || one.Count != 1 || one.Updates[0].Version != "2.29.3" {
		t.Errorf("model json = %+v", one)
	}

	optional, _ := s.Filter("optional")
	data, _ = s.ModelJSON(m, optional)
	if !bytes.Contains(data, []byte(`"updates": []`)) {
		t.Errorf("empty result should be [], not null:\n%s", data)
	}
}

func TestSlugs(t *testing.T) {
	for in, want := range map[string]string{"R650 XS": "r650xs", "MX740c": "mx740c", "R750 xa": "r750xa"} {
		if got := ModelSlug(in); got != want {
			t.Errorf("ModelSlug(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Network / RDMA":                  "network-rdma",
		"iDRAC with Lifecycle Controller": "idrac-with-lifecycle-controller",
		"  SAS RAID ":                     "sas-raid",
		"BIOS":                            "bios",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateKeepsRunes(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	if got := truncate(s, 5); got != "éé" {
		t.Errorf("truncate = %q", got)
	}
}
