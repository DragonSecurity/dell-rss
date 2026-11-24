package feed

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"strings"
	"time"
)

const rssTimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"

// maxHistory bounds how much revision history goes into each feed item.
const maxHistory = 2000

type rss struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Atom    string   `xml:"xmlns:atom,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title         string   `xml:"title"`
	Link          string   `xml:"link"`
	Self          *atomRef `xml:"atom:link,omitempty"`
	Description   string   `xml:"description"`
	Language      string   `xml:"language"`
	LastBuildDate string   `xml:"lastBuildDate"`
	TTL           int      `xml:"ttl"`
	Items         []item   `xml:"item"`
}

type atomRef struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Category    string `xml:"category"`
	PubDate     string `xml:"pubDate,omitempty"`
	GUID        guid   `xml:"guid"`
}

type guid struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

// Title is the human name of a model's (optionally filtered) feed.
func Title(m *Model, f Filter) string {
	if f.Label == "" {
		return fmt.Sprintf("Dell %s %s Updates", m.Brand, m.Name)
	}
	return fmt.Sprintf("Dell %s %s %s Updates", m.Brand, m.Name, f.Label)
}

// SupportURL is Dell's drivers page for a model.
func SupportURL(m *Model) string {
	return fmt.Sprintf("https://www.dell.com/support/product-details/en-ie/product/poweredge-%s/drivers", m.Slug)
}

// RSS renders an RSS 2.0 feed. selfURL may be empty.
func (s *Snapshot) RSS(m *Model, f Filter, selfURL string) ([]byte, error) {
	feed := rss{
		Version: "2.0",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: channel{
			Title:       Title(m, f),
			Link:        SupportURL(m),
			Description: fmt.Sprintf("Firmware and driver updates for Dell %s %s from the Dell enterprise catalog", m.Brand, m.Name),
			Language:    "en-ie",
			// Deterministic, so identical catalogs render identical bytes.
			LastBuildDate: s.Modified.UTC().Format(rssTimeFormat),
			TTL:           60,
		},
	}
	if selfURL != "" {
		feed.Channel.Self = &atomRef{Href: selfURL, Rel: "self", Type: "application/rss+xml"}
	}

	for _, u := range f.Apply(m.Updates) {
		link := u.InfoURL
		if link == "" {
			link = u.DownloadURL
		}
		it := item{
			Title:       strings.TrimSpace(u.Name + " - " + u.Version),
			Link:        link,
			Description: describe(m, u),
			Category:    u.Category,
			GUID:        guid{Value: u.ID},
		}
		if !u.Posted.IsZero() {
			it.PubDate = u.Posted.UTC().Format(rssTimeFormat)
		}
		feed.Channel.Items = append(feed.Channel.Items, it)
	}

	data, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(data, '\n')...), nil
}

// describe builds the item body as escaped HTML, which feed readers render.
func describe(m *Model, u *Update) string {
	var b strings.Builder
	row := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "<strong>%s:</strong> %s<br>\n", label, html.EscapeString(value))
		}
	}

	row("Model", m.Brand+" "+m.Name)
	row("Version", u.Version)
	row("Category", u.Category)
	row("Importance", u.Importance)
	if !u.Released.IsZero() {
		row("Released", u.Released.Format(time.DateOnly))
	}
	if u.RebootRequired {
		row("Reboot required", "yes")
	}
	fmt.Fprintf(&b, "<strong>Download:</strong> <a href=\"%s\">%s</a><br>\n",
		html.EscapeString(u.DownloadURL), html.EscapeString(fileName(u.DownloadURL)))
	if u.InfoURL != "" {
		fmt.Fprintf(&b, "<strong>Details:</strong> <a href=\"%s\">Dell support</a><br>\n", html.EscapeString(u.InfoURL))
	}

	if u.Description != "" {
		fmt.Fprintf(&b, "<p>%s</p>\n", paragraphs(u.Description))
	}
	if u.RevisionHistory != "" {
		history := u.RevisionHistory
		if len(history) > maxHistory {
			history = truncate(history, maxHistory) + " …"
		}
		fmt.Fprintf(&b, "<p>%s</p>\n", paragraphs(history))
	}
	return b.String()
}

func paragraphs(text string) string {
	return strings.ReplaceAll(html.EscapeString(strings.TrimSpace(text)), "\n", "<br>\n")
}

// truncate cuts at a rune boundary at or before n bytes.
func truncate(s string, n int) string {
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func fileName(link string) string {
	if i := strings.LastIndexByte(link, '/'); i >= 0 && i < len(link)-1 {
		return link[i+1:]
	}
	return link
}

// ModelSummary is the /api/models entry for one model.
type ModelSummary struct {
	Slug    string     `json:"slug"`
	Name    string     `json:"name"`
	Brand   string     `json:"brand"`
	Updates int        `json:"updates"`
	Latest  *time.Time `json:"latest,omitempty"`
	RSS     string     `json:"rss"`
	API     string     `json:"api"`
}

// CatalogSummary describes the catalog a response was built from.
type CatalogSummary struct {
	Version   string    `json:"version,omitempty"`
	ReleaseID string    `json:"release_id,omitempty"`
	Published time.Time `json:"published,omitzero"`
	Modified  time.Time `json:"modified"`
	Fetched   time.Time `json:"fetched"`
}

func (s *Snapshot) catalogSummary() CatalogSummary {
	return CatalogSummary{
		Version:   s.Manifest.Version,
		ReleaseID: s.Manifest.ReleaseID,
		Published: s.Manifest.Date,
		Modified:  s.Modified,
		Fetched:   s.Info.FetchedAt,
	}
}

// ModelsJSON renders /api/models.
func (s *Snapshot) ModelsJSON(defaultModel string) ([]byte, error) {
	out := struct {
		Catalog CatalogSummary `json:"catalog"`
		Default string         `json:"default,omitempty"`
		Models  []ModelSummary `json:"models"`
	}{Catalog: s.catalogSummary(), Default: defaultModel, Models: []ModelSummary{}}

	for _, m := range s.Models {
		sum := ModelSummary{
			Slug:    m.Slug,
			Name:    m.Name,
			Brand:   m.Brand,
			Updates: len(m.Updates),
			RSS:     "/" + m.Slug + "/rss.xml",
			API:     "/api/" + m.Slug,
		}
		if latest := m.Latest(); !latest.IsZero() {
			sum.Latest = &latest
		}
		out.Models = append(out.Models, sum)
	}
	return marshal(out)
}

// ModelJSON renders /api/{model} and /api/{model}/{filter}.
func (s *Snapshot) ModelJSON(m *Model, f Filter) ([]byte, error) {
	type filterJSON struct {
		Slug  string `json:"slug"`
		Label string `json:"label"`
		Count int    `json:"count"`
		RSS   string `json:"rss"`
		API   string `json:"api"`
	}

	updates := f.Apply(m.Updates)
	out := struct {
		Catalog CatalogSummary `json:"catalog"`
		Model   string         `json:"model"`
		Name    string         `json:"name"`
		Brand   string         `json:"brand"`
		Title   string         `json:"title"`
		Filter  string         `json:"filter,omitempty"`
		Filters []filterJSON   `json:"filters"`
		Count   int            `json:"count"`
		Updates []*Update      `json:"updates"`
	}{
		Catalog: s.catalogSummary(),
		Model:   m.Slug,
		Name:    m.Name,
		Brand:   m.Brand,
		Title:   Title(m, f),
		Filter:  f.Slug,
		Filters: []filterJSON{},
		Count:   len(updates),
		Updates: updates,
	}
	if out.Updates == nil {
		out.Updates = []*Update{}
	}
	for _, fl := range s.Filters(m) {
		out.Filters = append(out.Filters, filterJSON{
			Slug: fl.Slug, Label: fl.Label, Count: fl.Count,
			RSS: "/" + m.Slug + "/" + fl.Slug + "/rss.xml",
			API: "/api/" + m.Slug + "/" + fl.Slug,
		})
	}
	return marshal(out)
}

func marshal(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
