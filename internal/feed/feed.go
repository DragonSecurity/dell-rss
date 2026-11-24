// Package feed indexes a parsed catalog by server model and renders the
// per-model views (RSS and JSON) the HTTP server exposes.
package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dragonsecurity/dell-rss/internal/catalog"
)

// DefaultBrands are the catalog brands treated as PowerEdge servers.
// "Enterprise Servers" is how Dell files the 13th-generation PowerEdge
// models (R630, R730, ...).
var DefaultBrands = []string{"PowerEdge", "Enterprise Servers"}

var downloadBase = &url.URL{Scheme: "https", Host: "downloads.dell.com", Path: "/"}

// Update is a single package, shared by every model that supports it.
type Update struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Version         string    `json:"version"`
	ReleaseID       string    `json:"release_id,omitempty"`
	Category        string    `json:"category"`
	CategorySlug    string    `json:"category_slug"`
	Importance      string    `json:"importance"`
	Type            string    `json:"type,omitempty"`
	Posted          time.Time `json:"posted"`
	Released        time.Time `json:"released,omitzero"`
	DownloadURL     string    `json:"download_url"`
	InfoURL         string    `json:"info_url,omitempty"`
	Size            int64     `json:"size,omitempty"`
	RebootRequired  bool      `json:"reboot_required"`
	Description     string    `json:"description,omitempty"`
	RevisionHistory string    `json:"revision_history,omitempty"`
}

// Model is one server model and the updates that apply to it, newest first.
type Model struct {
	Slug    string
	Name    string
	Brand   string
	Updates []*Update
}

// Latest is the newest update's posted date.
func (m *Model) Latest() time.Time {
	if len(m.Updates) == 0 {
		return time.Time{}
	}
	return m.Updates[0].Posted
}

// Snapshot is an immutable, indexed view of one catalog download.
type Snapshot struct {
	ID         string // identifies the catalog bytes and the renderer
	Manifest   catalog.Manifest
	Info       catalog.Info
	Modified   time.Time
	Models     []*Model // sorted by slug
	Components int

	models     map[string]*Model
	categories map[string]string // slug -> display name
}

// Build indexes a catalog. renderVersion is mixed into the snapshot ID so a
// new release of this service invalidates clients' cached ETags.
func Build(c *catalog.Catalog, info catalog.Info, brands []string, renderVersion string) (*Snapshot, error) {
	allowed := make(map[string]bool, len(brands))
	for _, b := range brands {
		allowed[strings.ToLower(strings.TrimSpace(b))] = true
	}

	s := &Snapshot{
		Manifest:   c.Manifest,
		Info:       info,
		Modified:   info.Modified(),
		models:     make(map[string]*Model),
		categories: make(map[string]string),
	}

	seen := make(map[string]bool)

	for _, comp := range c.Components {
		if comp.Path == "" {
			continue
		}

		var systems []catalog.System
		for _, sys := range comp.Systems {
			if allowed[strings.ToLower(sys.Brand)] && ModelSlug(sys.Model) != "" {
				systems = append(systems, sys)
			}
		}
		if len(systems) == 0 {
			continue
		}

		update, ok := newUpdate(comp)
		if !ok || seen[update.ID] {
			continue
		}
		seen[update.ID] = true
		s.Components++

		if update.CategorySlug != "" {
			s.categories[update.CategorySlug] = update.Category
		}

		attached := make(map[string]bool)
		for _, sys := range systems {
			slug := ModelSlug(sys.Model)
			if attached[slug] {
				continue // the same model listed twice, e.g. "R750 xa" and "R750XA"
			}
			attached[slug] = true

			m := s.models[slug]
			if m == nil {
				m = &Model{Slug: slug, Name: sys.Model, Brand: sys.Brand}
				s.models[slug] = m
				s.Models = append(s.Models, m)
			}
			m.Updates = append(m.Updates, update)
		}
	}

	if len(s.Models) == 0 {
		return nil, fmt.Errorf("no updates found for brands %s", strings.Join(brands, ", "))
	}

	slices.SortFunc(s.Models, func(a, b *Model) int { return strings.Compare(a.Slug, b.Slug) })
	for _, m := range s.Models {
		slices.SortStableFunc(m.Updates, func(a, b *Update) int { return b.Posted.Compare(a.Posted) })
	}

	sum := sha256.Sum256([]byte(info.SHA256 + "|" + renderVersion + "|" + strings.Join(brands, ",")))
	s.ID = hex.EncodeToString(sum[:8])

	return s, nil
}

func newUpdate(c catalog.Component) (*Update, bool) {
	path, err := url.Parse(c.Path)
	if err != nil {
		return nil, false
	}
	link := downloadBase.ResolveReference(path).String()

	// Same identity as the original single-model feed, so existing
	// subscribers do not see every item again after the upgrade.
	hash := sha256.Sum256([]byte(link + "|" + c.Version))

	u := &Update{
		ID:              hex.EncodeToString(hash[:]),
		Name:            c.Name,
		Version:         c.Version,
		ReleaseID:       c.ReleaseID,
		Category:        c.Category,
		CategorySlug:    Slug(c.Category),
		Importance:      c.Criticality,
		Type:            c.ComponentType,
		Posted:          c.Posted,
		Released:        c.Released,
		DownloadURL:     link,
		Size:            c.Size,
		RebootRequired:  c.RebootRequired,
		Description:     c.Description,
		RevisionHistory: c.RevisionHistory,
	}
	if u.Posted.IsZero() {
		u.Posted = c.Released
	}
	if info, err := url.Parse(c.InfoURL); err == nil && (info.Scheme == "https" || info.Scheme == "http") && info.Host != "" {
		u.InfoURL = info.String()
	}
	return u, true
}

// Model looks a model up by slug (case- and punctuation-insensitive).
func (s *Snapshot) Model(name string) (*Model, bool) {
	m, ok := s.models[ModelSlug(name)]
	return m, ok
}

// Filter selects a subset of a model's updates.
type Filter struct {
	Slug  string // "" means every update
	Label string // e.g. "BIOS", "Urgent"
	match func(*Update) bool
}

// All is the unfiltered view.
var All = Filter{}

// Match reports whether the update belongs in the filtered view.
func (f Filter) Match(u *Update) bool { return f.match == nil || f.match(u) }

// Apply returns the matching updates in order.
func (f Filter) Apply(updates []*Update) []*Update {
	if f.match == nil {
		return updates
	}
	var out []*Update
	for _, u := range updates {
		if f.match(u) {
			out = append(out, u)
		}
	}
	return out
}

var importances = []string{"urgent", "recommended", "optional"}

// aliases are short names for categories whose catalog names are unwieldy.
var aliases = map[string]string{
	"idrac": "idrac-with-lifecycle-controller",
}

// Filter resolves a filter name: an importance level (urgent, recommended,
// optional), a category slug (bios, network, sas-raid, ...), or an alias
// (idrac). Unknown names return false.
func (s *Snapshot) Filter(name string) (Filter, bool) {
	name = strings.ToLower(name)

	if slices.Contains(importances, name) {
		return Filter{
			Slug:  name,
			Label: strings.ToUpper(name[:1]) + name[1:],
			match: func(u *Update) bool { return strings.EqualFold(u.Importance, name) },
		}, true
	}

	category := name
	if target, ok := aliases[name]; ok {
		category = target
	}
	display, ok := s.categories[category]
	if !ok {
		return Filter{}, false
	}
	label := display
	if name == "idrac" {
		label = "iDRAC"
	}
	return Filter{
		Slug:  name,
		Label: label,
		match: func(u *Update) bool { return u.CategorySlug == category },
	}, true
}

// FilterLink is a filter worth advertising for a model.
type FilterLink struct {
	Slug  string
	Label string
	Count int
}

// Filters lists the filters that select at least one of the model's updates:
// importance levels first, then categories by name.
func (s *Snapshot) Filters(m *Model) []FilterLink {
	var out []FilterLink
	add := func(name string) {
		if f, ok := s.Filter(name); ok {
			if n := len(f.Apply(m.Updates)); n > 0 {
				out = append(out, FilterLink{Slug: f.Slug, Label: f.Label, Count: n})
			}
		}
	}

	for _, name := range importances {
		add(name)
	}

	reverse := make(map[string]string, len(aliases))
	for alias, target := range aliases {
		reverse[target] = alias
	}

	var cats []string
	for slug := range s.categories {
		cats = append(cats, slug)
	}
	slices.Sort(cats)
	for _, slug := range cats {
		if alias, ok := reverse[slug]; ok {
			slug = alias
		}
		add(slug)
	}
	return out
}

// ModelSlug normalises a model name for URLs: "R650 XS" -> "r650xs".
func ModelSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Slug normalises a category name: "Network / RDMA" -> "network-rdma".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	return b.String()
}
