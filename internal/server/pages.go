package server

import (
	"bytes"
	"net/http"
	"time"
	"unicode"

	"github.com/dragonsecurity/dell-rss/internal/feed"
)

const recentOnDashboard = 15

type modelGroup struct {
	Label  string
	Models []*feed.Model
}

type pageData struct {
	Version  string
	Snap     *feed.Snapshot
	Checked  time.Time
	Default  *feed.Model
	Recent   []*feed.Update
	Groups   []modelGroup
	Model    *feed.Model
	Filter   feed.Filter
	Filters  []feed.FilterLink
	Updates  []*feed.Update
	Title    string
	FeedPath string
	APIPath  string
}

func (s *Server) baseData(snap *feed.Snapshot) *pageData {
	return &pageData{
		Version: s.cfg.Version,
		Snap:    snap,
		Checked: time.Unix(0, s.checked.Load()),
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.ready(w, r)
	if !ok {
		return
	}

	data := s.baseData(snap)
	data.Title = "Dell PowerEdge updates"
	if m, ok := snap.Model(s.cfg.DefaultModel); ok {
		data.Default = m
		data.Recent = m.Updates[:min(recentOnDashboard, len(m.Updates))]
		data.Filters = snap.Filters(m)
	}
	data.Groups = groupModels(snap.Models)

	s.page(w, http.StatusOK, "dashboard", data)
}

func (s *Server) modelPage(w http.ResponseWriter, r *http.Request, modelName, filterName string) {
	snap, ok := s.ready(w, r)
	if !ok {
		return
	}
	model, filter, ok := s.resolve(w, r, snap, modelName, filterName)
	if !ok {
		return
	}

	data := s.baseData(snap)
	data.Model = model
	data.Filter = filter
	data.Filters = snap.Filters(model)
	data.Updates = filter.Apply(model.Updates)
	data.Title = feed.Title(model, filter)
	data.FeedPath = "/" + model.Slug + "/rss.xml"
	data.APIPath = "/api/" + model.Slug
	if filter.Slug != "" {
		data.FeedPath = "/" + model.Slug + "/" + filter.Slug + "/rss.xml"
		data.APIPath += "/" + filter.Slug
	}

	s.page(w, http.StatusOK, "model", data)
}

// page renders into a buffer first so a template error is a clean 500
// rather than half a page.
func (s *Server) page(w http.ResponseWriter, code int, name string, data *pageData) {
	if data == nil {
		data = s.baseData(s.snapshot.Load())
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("template failed", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

// groupModels buckets models by series prefix: R, T, C, M/MX, XE, XR, ...
func groupModels(models []*feed.Model) []modelGroup {
	var groups []modelGroup
	index := map[string]int{}
	for _, m := range models {
		label := seriesOf(m.Name)
		i, ok := index[label]
		if !ok {
			i = len(groups)
			index[label] = i
			groups = append(groups, modelGroup{Label: label})
		}
		groups[i].Models = append(groups[i].Models, m)
	}
	return groups
}

func seriesOf(name string) string {
	prefix := []rune{}
	for _, r := range name {
		if !unicode.IsLetter(r) {
			break
		}
		prefix = append(prefix, unicode.ToUpper(r))
	}
	if len(prefix) == 0 {
		return "Other"
	}
	return string(prefix) + " series"
}
