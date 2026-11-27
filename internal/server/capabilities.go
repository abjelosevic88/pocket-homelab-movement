package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-movement/internal/store"
)

// What Pocket Search, Pocket Day and Pocket Hub ask Movement (the
// capabilities value in core.go): one route each, in the shape every Pocket
// app answers in. They read what the Places page, the timeline and the day
// summary read, as the request's person, and change nothing. Location is the
// most private thing on the box, so these answers carry names, times,
// distances and counts only: no coordinates, no route, no point, no address
// of where the person is now. Hidden places stay hidden everywhere here.

// placeWords are the place categories as the Places page names them.
var placeWords = map[string]string{"home": "Home", "work": "Work", "school": "School", "family": "Family & friends", "food": "Food & drink", "shop": "Shopping",
	"sport": "Sport", "health": "Health", "travel": "Travel", "leisure": "Leisure", "nature": "Nature", "other": "Other"}

// modeWords are the transport modes as the timeline names them.
var modeWords = map[string]string{"walking": "Walking", "running": "Running", "cycling": "Cycling", "driving": "Driving", "bus": "Bus", "train": "Train",
	"boat": "Boat", "flying": "Flying"}

// unnamedPlace is how the timeline words a stop whose place has no name yet.
const unnamedPlace = "Unnamed place"

// searchVisits is how many of a place's latest visits come with it.
const searchVisits = 3

// hasWord reports whether there is a letter or a digit to look for.
func hasWord(q string) bool {
	return strings.IndexFunc(q, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// dots joins the parts of a line that are there: "2 h 15 min · Sarajevo".
func dots(parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// plural is "1 place", "3 places".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// fmtDist is metres as the app shows them: "12.4 km", "850 m", "7.7 mi".
func fmtDist(m float64, unit string) string {
	if m <= 0 {
		if unit == "mi" {
			return "0 mi"
		}
		return "0 km"
	}
	if unit == "mi" {
		mi := m / 1609.344
		switch {
		case mi < 0.2:
			return fmt.Sprintf("%d yd", int(math.Round(m*1.09361)))
		case mi < 10:
			return fmt.Sprintf("%.1f mi", mi)
		}
		return fmt.Sprintf("%d mi", int(math.Round(mi)))
	}
	if m < 1000 {
		return fmt.Sprintf("%d m", int(math.Round(m)))
	}
	if km := m / 1000; km < 10 {
		return fmt.Sprintf("%.1f km", km)
	}
	return fmt.Sprintf("%d km", int(math.Round(m/1000)))
}

// fmtDur is seconds as the app shows them: "2 h 15 min", "45 min", "3 d 4 h".
func fmtDur(s int64) string {
	if s < 0 {
		s = 0
	}
	if s < 60 {
		return fmt.Sprintf("%d s", s)
	}
	m := int64(math.Round(float64(s) / 60))
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	h, mm := m/60, m%60
	if h < 48 {
		if mm > 0 {
			return fmt.Sprintf("%d h %d min", h, mm)
		}
		return fmt.Sprintf("%d h", h)
	}
	d, hh := h/24, h%24
	if hh > 0 {
		return fmt.Sprintf("%d d %d h", d, hh)
	}
	return fmt.Sprintf("%d d", d)
}

// placeCategories are the categories whose name has the words: "food" and
// "drink" both find the places filed under Food & drink.
func placeCategories(q string) []string {
	lq := strings.ToLower(q)
	var out []string
	for id, label := range placeWords {
		if strings.Contains(strings.ToLower(label), lq) {
			out = append(out, id)
		}
	}
	return out
}

// visitName is a stop in a word or two, as the timeline names it.
func visitName(v *store.Visit) string {
	if n := strings.TrimSpace(v.DisplayName()); n != "" {
		return n
	}
	return unnamedPlace
}

// ---------- search ----------

// pocketSearch answers Pocket Search: the person's places (name, address,
// city, category, notes), each followed by its latest visits, so one place
// visited every day does not fill the list on its own. The places with the
// words in their name come first, then the most visited. Hidden places are
// never found, and words without a letter or a digit find nothing.
func (s *Server) pocketSearch(r *http.Request, q string, limit int) ([]core.SearchItem, error) {
	if !hasWord(q) {
		return nil, nil
	}
	ctx := r.Context()
	uid := userFrom(ctx).ID
	_, loc := s.userLoc(ctx, uid)
	places, err := s.store.FindPlaces(ctx, uid, q, placeCategories(q), limit)
	if err != nil {
		return nil, err
	}
	if len(places) == 0 {
		return nil, nil
	}
	// the places first: each brings at most a few of its visits, fewer when
	// many places match, none when the places alone fill the list
	per := min(searchVisits, max(0, (limit-len(places))/len(places)))
	out := []core.SearchItem{}
	for i := range places {
		p := &places[i]
		id := strconv.FormatInt(p.ID, 10)
		var visited string
		if p.Visits > 0 {
			visited = plural(p.Visits, "visit", "visits")
		}
		it := core.SearchItem{ID: "place:" + id, Title: p.Name, Kind: "place", URL: "/places/" + id,
			Snippet: dots(firstNonEmpty(p.Address, dots(placeWords[p.Category], p.City)), visited)}
		if strings.TrimSpace(it.Title) == "" {
			it.Title = unnamedPlace
		}
		if p.LastVisit > 0 {
			it.Date = time.Unix(p.LastVisit, 0).In(loc).Format("2006-01-02")
		}
		out = append(out, it)
		if per == 0 || p.Visits == 0 {
			continue
		}
		visits, err := s.store.Visits(ctx, uid, store.VisitQuery{PlaceID: p.ID, NoDecline: true, Desc: true, Limit: per})
		if err != nil {
			return nil, err
		}
		for j := range visits {
			v := &visits[j]
			start := time.Unix(v.StartTS, 0).In(loc)
			day := start.Format("2006-01-02")
			out = append(out, core.SearchItem{ID: "visit:" + strconv.FormatInt(v.ID, 10), Title: visitName(v), Kind: "visit", Date: day, URL: "/timeline/" + day,
				Snippet: dots(start.Format("Mon 2 Jan 2006, 15:04"), fmtDur(v.EndTS-v.StartTS))})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---------- day ----------

// movementDay is a day as the timeline shows it, in the contract's words,
// with the totals of its summary.
type movementDay struct {
	items    []core.DayItem
	distance float64 // metres
	moving   int64   // seconds
	places   int
}

// readDay reads the stops and journeys of the day [from, to] in loc the way
// /day/{date} reads them, declined visits and visits at hidden places left
// out. Distance and time moving come from the app's own day summary when the
// day is cut as the person's own days are (own), else from the journeys that
// start that day. A stop already going at midnight starts at 00:00; one that
// goes on past the end of the day has no end time.
func (s *Server) readDay(ctx context.Context, uid int64, date string, from, to int64, loc, own *time.Location, unit string) (*movementDay, error) {
	visits, err := s.store.Visits(ctx, uid, store.VisitQuery{From: from, To: to, NoDecline: true})
	if err != nil {
		return nil, err
	}
	tracks, err := s.store.Tracks(ctx, uid, from, to, false)
	if err != nil {
		return nil, err
	}
	clocks := func(start, end int64) (string, string) {
		a, b := "00:00", ""
		if start >= from {
			a = time.Unix(start, 0).In(loc).Format("15:04")
		}
		if end <= to {
			b = time.Unix(end, 0).In(loc).Format("15:04")
		}
		return a, b
	}
	inDay := func(start, end int64) float64 {
		return math.Round(float64(min(end, to+1)-max(start, from)) / 60)
	}
	url := "/timeline/" + date
	d := &movementDay{}
	seen := map[int64]bool{}
	for i := range visits {
		v := &visits[i]
		if v.Place != nil && v.Place.Hidden {
			continue
		}
		it := core.DayItem{ID: "visit:" + strconv.FormatInt(v.ID, 10), Title: visitName(v), Kind: "visit", Minutes: inDay(v.StartTS, v.EndTS), URL: url,
			Note: strings.TrimSpace(v.Notes)}
		it.Time, it.End = clocks(v.StartTS, v.EndTS)
		var city, saved string
		if v.Place != nil {
			city = v.Place.City
			if v.Place.Kind == store.PlaceSaved {
				saved = "saved place"
			}
		}
		it.Sub = dots(fmtDur(v.EndTS-v.StartTS), city, saved)
		d.items = append(d.items, it)
		switch {
		case v.PlaceID == nil:
			d.places++
		case !seen[*v.PlaceID]:
			seen[*v.PlaceID] = true
			d.places++
		}
	}
	var dist float64
	var moving int64
	for _, t := range tracks {
		it := core.DayItem{ID: "journey:" + strconv.FormatInt(t.ID, 10), Title: firstNonEmpty(modeWords[t.Mode], "Moving"), Kind: "journey",
			Minutes: inDay(t.StartTS, t.EndTS), Qty: round1(t.Distance / 1000), URL: url, Sub: dots(fmtDist(t.Distance, unit), fmtDur(t.EndTS-t.StartTS))}
		it.Time, it.End = clocks(t.StartTS, t.EndTS)
		d.items = append(d.items, it)
		if t.StartTS >= from {
			dist += t.Distance
			moving += t.EndTS - t.StartTS
		}
	}
	d.distance, d.moving = dist, moving
	if d0, err := time.ParseInLocation("2006-01-02", date, own); err == nil && d0.Unix() == from && d0.AddDate(0, 0, 1).Unix()-1 == to {
		if days, _ := s.store.Days(ctx, uid, date, date); len(days) == 1 && (days[0].Distance > 0 || days[0].MovingS > 0) {
			d.distance, d.moving = days[0].Distance, days[0].MovingS
		}
	}
	return d, nil
}

// pocketDay answers Pocket Day: the day's stops and journeys as the timeline
// lists them, with the line its summary says: "24 km travelled, 1 h 10 min
// moving, 3 places". The day is cut in the caller's time zone when it says
// one, else in the person's own (Settings).
func (s *Server) pocketDay(r *http.Request, date string, tz *time.Location) (*core.Day, error) {
	ctx := r.Context()
	uid := userFrom(ctx).ID
	st, own := s.userLoc(ctx, uid)
	loc := own
	if tz != nil {
		loc = tz
	}
	d0, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return nil, err
	}
	d, err := s.readDay(ctx, uid, date, d0.Unix(), d0.AddDate(0, 0, 1).Unix()-1, loc, own, st.DistanceUnit)
	if err != nil {
		return nil, err
	}
	if len(d.items) == 0 && d.distance <= 0 {
		return nil, nil
	}
	var parts []string
	if d.distance > 0 {
		parts = append(parts, fmtDist(d.distance, st.DistanceUnit)+" travelled")
	}
	if d.moving >= 60 {
		parts = append(parts, fmtDur(d.moving)+" moving")
	}
	if d.places > 0 {
		parts = append(parts, plural(d.places, "place", "places"))
	}
	day := &core.Day{Date: date, Summary: strings.Join(parts, ", "), Items: d.items}
	if d.distance > 0 {
		day.Metrics = append(day.Metrics, core.Metric{Label: "Distance", Value: fmtDist(d.distance, st.DistanceUnit)})
	}
	if d.places > 0 {
		day.Metrics = append(day.Metrics, core.Metric{Label: "Places", Value: strconv.Itoa(d.places)})
	}
	return day, nil
}

// ---------- widget ----------

// pocketWidget answers Pocket Hub with today so far: how far and how many
// places, "24 km, 3 places today". It does not say where the person is now,
// though the map's first page does: a home screen may be on a shared
// display, and the place someone is at this minute is not for every passer-by.
// No place names either, for the same reason. Nothing today, nothing to show.
func (s *Server) pocketWidget(r *http.Request) (*core.WidgetData, error) {
	ctx := r.Context()
	uid := userFrom(ctx).ID
	st, loc := s.userLoc(ctx, uid)
	now := time.Now().In(loc)
	d0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	date := d0.Format("2006-01-02")
	d, err := s.readDay(ctx, uid, date, d0.Unix(), d0.AddDate(0, 0, 1).Unix()-1, loc, loc, st.DistanceUnit)
	if err != nil {
		return nil, err
	}
	if d.distance <= 0 && d.places == 0 {
		return nil, nil
	}
	w := &core.WidgetData{URL: "/timeline/" + date, Items: []core.WidgetItem{}}
	var parts []string
	if d.distance > 0 {
		parts = append(parts, fmtDist(d.distance, st.DistanceUnit))
		w.Metrics = append(w.Metrics, core.Metric{Label: "Distance", Value: fmtDist(d.distance, st.DistanceUnit)})
	}
	if d.places > 0 {
		parts = append(parts, plural(d.places, "place", "places"))
		w.Metrics = append(w.Metrics, core.Metric{Label: "Places", Value: strconv.Itoa(d.places)})
	}
	w.Line = strings.Join(parts, ", ") + " today"
	return w, nil
}
