package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-movement/internal/config"
	"github.com/abjelosevic88/pocket-homelab-movement/internal/store"
)

// capBox is a moveBox with alice's history in it and personal tokens for
// alice and bob (signed in as themselves).
type capBox struct {
	*moveBox
	aliceTok, bobTok string
	loc              *time.Location
	ids              map[string]int64
}

const capDay = "2026-11-20"

// newCapBox gives alice (in Sarajevo) a day of stops and journeys on
// 2026-11-20, earlier visits to a café, a hidden place, raw points, and a
// little of today; bob has nothing.
func newCapBox(t *testing.T) *capBox {
	t.Helper()
	b := &capBox{moveBox: newMoveBox(t), ids: map[string]int64{}}
	ctx := t.Context()
	st := b.s.store
	b.aliceTok, _, _ = st.CreateAPIToken(ctx, b.alice.ID, "test")
	b.bobTok, _, _ = st.CreateAPIToken(ctx, b.bob.ID, "test")
	set, _ := st.GetSettings(ctx, b.alice.ID)
	set.Timezone = "Europe/Sarajevo"
	if err := st.SaveUserSettings(ctx, b.alice.ID, set); err != nil {
		t.Fatal(err)
	}
	b.loc, _ = time.LoadLocation("Europe/Sarajevo")
	at := func(day, hhmm string) int64 {
		tm, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hhmm, b.loc)
		if err != nil {
			t.Fatal(err)
		}
		return tm.Unix()
	}
	place := func(key string, p store.Place) {
		p.UserID = b.alice.ID
		if err := st.CreatePlace(ctx, &p); err != nil {
			t.Fatal(err)
		}
		b.ids[key] = p.ID
	}
	place("home", store.Place{Name: "Home", Kind: store.PlaceSaved, Category: "home", Lat: 43.8512, Lng: 18.3921, Radius: 80, Address: "Ferhadija 12", City: "Sarajevo"})
	place("cafe", store.Place{Name: "Cafe Tito", Kind: store.PlaceDiscovered, Category: "food", Lat: 43.8563, Lng: 18.4011, Radius: 40, City: "Sarajevo", Notes: "the best burek"})
	place("clinic", store.Place{Name: "Clinic Vita", Kind: store.PlaceSaved, Category: "health", Lat: 43.8601, Lng: 18.4102, Radius: 50, City: "Sarajevo", Hidden: true})
	place("park", store.Place{Name: "Vilsonovo", Kind: store.PlaceDiscovered, Category: "nature", Lat: 43.8501, Lng: 18.3801, City: "Sarajevo"})
	// the journeys first: a rebuild drops the visits it did not make
	todayStart := func() time.Time {
		n := time.Now().In(b.loc)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, b.loc)
	}()
	if err := st.ApplyRebuild(ctx, b.alice.ID, store.Rebuild{From: at("2026-11-19", "00:00"), To: at("2026-11-22", "00:00"), Tracks: []store.Track{
		{StartTS: at(capDay, "08:00"), EndTS: at(capDay, "08:20"), Distance: 5400, Mode: "driving", StartLat: 43.8512, StartLng: 18.3921, EndLat: 43.8563, EndLng: 18.4011, Polyline: "_p~iF~ps|U_ulLnnqC"},
		{StartTS: at(capDay, "09:35"), EndTS: at(capDay, "09:50"), Distance: 1200, Mode: "unknown", StartLat: 43.8563, StartLng: 18.4011, EndLat: 43.8601, EndLng: 18.4102},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyRebuild(ctx, b.alice.ID, store.Rebuild{From: todayStart.Unix() + 60, To: todayStart.Unix() + 120, Tracks: []store.Track{
		{StartTS: todayStart.Unix() + 60, EndTS: todayStart.Unix() + 120, Distance: 24300, Mode: "train"},
	}}); err != nil {
		t.Fatal(err)
	}
	visit := func(key, place string, from, to int64, status, name, notes string) {
		v := store.Visit{UserID: b.alice.ID, StartTS: from, EndTS: to, Status: status, Name: name, Notes: notes, Lat: 43.8512, Lng: 18.3921, Manual: true}
		if place != "" {
			id := b.ids[place]
			v.PlaceID = &id
		}
		if err := st.CreateVisit(ctx, &v); err != nil {
			t.Fatal(err)
		}
		if key != "" {
			b.ids[key] = v.ID
		}
	}
	visit("night", "home", at("2026-11-19", "22:00"), at(capDay, "08:00"), store.VisitConfirmed, "", "")
	visit("coffee", "cafe", at(capDay, "08:20"), at(capDay, "09:35"), store.VisitSuggested, "", "with Amar")
	visit("", "clinic", at(capDay, "10:00"), at(capDay, "11:00"), store.VisitConfirmed, "", "")
	visit("bench", "", at(capDay, "12:00"), at(capDay, "12:30"), store.VisitSuggested, "", "")
	visit("", "cafe", at(capDay, "13:00"), at(capDay, "13:30"), store.VisitDeclined, "", "")
	visit("evening", "home", at(capDay, "18:00"), at("2026-11-21", "07:00"), store.VisitConfirmed, "", "")
	for i, day := range []string{"2026-11-10", "2026-11-12", "2026-11-14", "2026-11-16"} {
		visit("cafe"+string(rune('a'+i)), "cafe", at(day, "09:00"), at(day, "09:45"), store.VisitConfirmed, "", "")
	}
	visit("", "park", at("2026-11-15", "16:00"), at("2026-11-15", "17:00"), store.VisitConfirmed, "", "")
	// today: home since yesterday, the café, the hidden clinic
	visit("", "home", todayStart.Unix()-3600, todayStart.Unix()+30, store.VisitConfirmed, "", "")
	visit("", "cafe", todayStart.Unix()+130, todayStart.Unix()+200, store.VisitConfirmed, "", "")
	visit("", "clinic", todayStart.Unix()+300, todayStart.Unix()+400, store.VisitConfirmed, "", "")
	if err := st.SaveDays(ctx, b.alice.ID, []store.Day{{Date: capDay, Points: 40, Distance: 7300, MovingS: 2100, Visits: 5, Places: []int64{b.ids["home"], b.ids["cafe"], b.ids["clinic"]}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertPoints(ctx, b.alice.ID, []store.Point{{TS: at(capDay, "08:10"), Lat: 43.8533, Lng: 18.3955, Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	return b
}

// get calls a route and decodes the answer.
func (b *capBox) get(tok, path string) (int, map[string]any, string) {
	b.t.Helper()
	code, body := b.do(tok, "GET", path, "", nil)
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	return code, m, body
}

func (b *capBox) ok(tok, path string) (map[string]any, string) {
	b.t.Helper()
	code, m, body := b.get(tok, path)
	if code != 200 {
		b.t.Fatalf("GET %s → %d %s", path, code, body)
	}
	return m, body
}

// items are the rows of a capability answer.
func items(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	l, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("no items list in %v", m)
	}
	out := []map[string]any{}
	for _, v := range l {
		out = append(out, v.(map[string]any))
	}
	return out
}

// noPlaceOnEarth fails when an answer carries a position, a route or a point.
func noPlaceOnEarth(t *testing.T, body string) {
	t.Helper()
	for _, no := range []string{"43.8", "18.3", "18.4", "lat", "lng", "polyline", "_p~if", "point"} {
		if strings.Contains(strings.ToLower(body), no) {
			t.Fatalf("the answer carries %q: %s", no, body)
		}
	}
}

func TestPocketSearch(t *testing.T) {
	b := newCapBox(t)
	id := func(k string) string { return strconv.FormatInt(b.ids[k], 10) }

	// a place by its name, with its latest three visits, newest first
	m, body := b.ok(b.aliceTok, "/api/v1/pocket/search?q=tito")
	noPlaceOnEarth(t, body)
	got := items(t, m)
	if len(got) != 4 {
		t.Fatalf("tito: %v", got)
	}
	today := time.Now().In(b.loc).Format("2006-01-02")
	if p := got[0]; p["id"] != "place:"+id("cafe") || p["kind"] != "place" || p["title"] != "Cafe Tito" || p["url"] != "/places/"+id("cafe") ||
		p["snippet"] != "Food & drink · Sarajevo · 6 visits" || p["date"] != today {
		t.Fatalf("place: %v", p)
	}
	if v := got[1]; v["kind"] != "visit" || v["date"] != today || v["url"] != "/timeline/"+today {
		t.Fatalf("today's visit: %v", v)
	}
	if v := got[2]; v["id"] != "visit:"+id("coffee") || v["kind"] != "visit" || v["title"] != "Cafe Tito" || v["date"] != capDay || v["url"] != "/timeline/"+capDay ||
		v["snippet"] != "Fri 20 Nov 2026, 08:20 · 1 h 15 min" {
		t.Fatalf("visit on %s: %v", capDay, v)
	}
	if got[3]["id"] != "visit:"+id("cafed") || got[3]["date"] != "2026-11-16" || got[3]["snippet"] != "Mon 16 Nov 2026, 09:00 · 45 min" {
		t.Fatalf("older visit: %v", got[3])
	}
	// the declined visit is not a visit
	for _, it := range got {
		if it["snippet"] == "Fri 20 Nov 2026, 13:00 · 30 min" {
			t.Fatalf("declined visit found: %v", it)
		}
	}
	// by address, notes and category name; one item without visits when asked for one
	for q, want := range map[string]string{"ferhadija": "Home", "burek": "Cafe Tito", "drink": "Cafe Tito", "nature": "Vilsonovo"} {
		got := items(t, mustOK(b, b.aliceTok, "/api/v1/pocket/search?q="+q+"&limit=1"))
		if len(got) != 1 || got[0]["title"] != want || got[0]["kind"] != "place" {
			t.Fatalf("%s: %v", q, got)
		}
	}
	if h := items(t, mustOK(b, b.aliceTok, "/api/v1/pocket/search?q=ferhadija"))[0]; h["snippet"] != "Ferhadija 12 · 3 visits" {
		t.Fatalf("home: %v", h)
	}
	// many places: every place before any visit
	got = items(t, mustOK(b, b.aliceTok, "/api/v1/pocket/search?q=sarajevo&limit=3"))
	if len(got) != 3 || got[0]["kind"] != "place" || got[1]["kind"] != "place" || got[2]["kind"] != "place" {
		t.Fatalf("sarajevo: %v", got)
	}
	got = items(t, mustOK(b, b.aliceTok, "/api/v1/pocket/search?q=sarajevo&limit=8"))
	places := 0
	for _, it := range got {
		if it["kind"] == "place" {
			places++
		}
		if it["title"] == "Clinic Vita" {
			t.Fatalf("a hidden place: %v", it)
		}
	}
	if places != 3 || len(got) != 6 {
		t.Fatalf("sarajevo, 8: %v", got)
	}
	// hidden places are never found, nor are their visits
	for _, q := range []string{"clinic", "vita", "health"} {
		if got := items(t, mustOK(b, b.aliceTok, "/api/v1/pocket/search?q="+q)); len(got) != 0 {
			t.Fatalf("%s: %v", q, got)
		}
	}
	// nothing to look for finds nothing, not everything
	for _, q := range []string{"", "??", "*", "%25", "_", "+", "%25%25"} {
		if got := items(t, mustOK(b, b.aliceTok, "/api/v1/pocket/search?q="+q)); len(got) != 0 {
			t.Fatalf("%q: %v", q, got)
		}
	}
	// another person's places never appear
	for _, q := range []string{"tito", "home", "sarajevo", "a"} {
		if got := items(t, mustOK(b, b.bobTok, "/api/v1/pocket/search?q="+q)); len(got) != 0 {
			t.Fatalf("bob finds alice's %q: %v", q, got)
		}
	}
	if code, _, _ := b.get("", "/api/v1/pocket/search?q=tito"); code != 401 {
		t.Fatalf("no one: %d", code)
	}
	// the old route answers as before (hidden places too, for the app itself)
	if old := mustOK(b, b.aliceTok, "/api/v1/search?q=clinic")["places"].([]any); len(old) != 1 {
		t.Fatalf("/search changed: %v", old)
	}
}

func mustOK(b *capBox, tok, path string) map[string]any {
	b.t.Helper()
	m, _ := b.ok(tok, path)
	return m
}

func TestPocketDay(t *testing.T) {
	b := newCapBox(t)
	id := func(k string) string { return strconv.FormatInt(b.ids[k], 10) }
	m, body := b.ok(b.aliceTok, "/api/v1/pocket/day/"+capDay)
	noPlaceOnEarth(t, body)
	if m["date"] != capDay || m["summary"] != "7.3 km travelled, 35 min moving, 3 places" {
		t.Fatalf("day: %v", m)
	}
	metrics, _ := m["metrics"].([]any)
	if len(metrics) != 2 || metrics[0].(map[string]any)["label"] != "Distance" || metrics[0].(map[string]any)["value"] != "7.3 km" ||
		metrics[1].(map[string]any)["label"] != "Places" || metrics[1].(map[string]any)["value"] != "3" {
		t.Fatalf("metrics: %v", metrics)
	}
	byID := map[string]map[string]any{}
	for _, it := range items(t, m) {
		byID[it["id"].(string)] = it
		if it["title"] == "Clinic Vita" || it["time"] == "13:00" || it["time"] == "10:00" {
			t.Fatalf("hidden or declined: %v", it)
		}
	}
	if len(byID) != 6 {
		t.Fatalf("items: %v", m["items"])
	}
	if n := byID["visit:"+id("night")]; n["title"] != "Home" || n["time"] != "00:00" || n["end"] != "08:00" || n["minutes"] != 480.0 || n["kind"] != "visit" ||
		n["sub"] != "10 h · Sarajevo · saved place" || n["url"] != "/timeline/"+capDay {
		t.Fatalf("night at home: %v", n)
	}
	if c := byID["visit:"+id("coffee")]; c["title"] != "Cafe Tito" || c["time"] != "08:20" || c["end"] != "09:35" || c["minutes"] != 75.0 || c["note"] != "with Amar" ||
		c["sub"] != "1 h 15 min · Sarajevo" {
		t.Fatalf("coffee: %v", c)
	}
	if u := byID["visit:"+id("bench")]; u["title"] != "Unnamed place" || u["time"] != "12:00" || u["end"] != "12:30" || u["sub"] != "30 min" {
		t.Fatalf("unnamed stop: %v", u)
	}
	if e := byID["visit:"+id("evening")]; e["time"] != "18:00" || e["end"] != nil || e["minutes"] != 360.0 {
		t.Fatalf("evening: %v", e)
	}
	var drive, walk map[string]any
	for _, it := range byID {
		if it["kind"] == "journey" {
			if it["time"] == "08:00" {
				drive = it
			} else {
				walk = it
			}
		}
	}
	if drive == nil || drive["title"] != "Driving" || drive["end"] != "08:20" || drive["qty"] != 5.4 || drive["minutes"] != 20.0 || drive["sub"] != "5.4 km · 20 min" ||
		!strings.HasPrefix(drive["id"].(string), "journey:") {
		t.Fatalf("drive: %v", drive)
	}
	if walk == nil || walk["title"] != "Moving" || walk["time"] != "09:35" || walk["qty"] != 1.2 {
		t.Fatalf("an unknown mode: %v", walk)
	}
	// in miles for someone who reads those
	set, _ := b.s.store.GetSettings(t.Context(), b.alice.ID)
	set.DistanceUnit = "mi"
	if err := b.s.store.SaveUserSettings(t.Context(), b.alice.ID, set); err != nil {
		t.Fatal(err)
	}
	if m := mustOK(b, b.aliceTok, "/api/v1/pocket/day/"+capDay); m["summary"] != "4.5 mi travelled, 35 min moving, 3 places" {
		t.Fatalf("miles: %v", m["summary"])
	}
	// cut in another zone, the day's distance is its journeys'
	m = mustOK(b, b.aliceTok, "/api/v1/pocket/day/"+capDay+"?tz=UTC")
	if m["summary"] != "4.1 mi travelled, 35 min moving, 3 places" {
		t.Fatalf("UTC: %v", m["summary"])
	}
	for _, it := range items(t, m) {
		if it["id"] == "journey:"+strconv.FormatInt(firstJourney(t, b), 10) && it["time"] != "07:00" {
			t.Fatalf("UTC clock: %v", it)
		}
	}
	// a day with nothing says nothing
	if m := mustOK(b, b.aliceTok, "/api/v1/pocket/day/2026-10-01"); m["summary"] != nil || len(items(t, m)) != 0 {
		t.Fatalf("empty day: %v", m)
	}
	if code, _, _ := b.get(b.aliceTok, "/api/v1/pocket/day/2026-13-01"); code != 400 {
		t.Fatalf("bad day: %d", code)
	}
	// another person's day is empty
	if m := mustOK(b, b.bobTok, "/api/v1/pocket/day/"+capDay); m["summary"] != nil || len(items(t, m)) != 0 {
		t.Fatalf("bob sees alice's day: %v", m)
	}
	// the app's own day answers as before (declined visits and hidden places included)
	if old := mustOK(b, b.aliceTok, "/api/v1/day/"+capDay)["entries"].([]any); len(old) != 8 {
		t.Fatalf("/day changed: %d entries", len(old))
	}
}

// firstJourney is the id of alice's drive on capDay.
func firstJourney(t *testing.T, b *capBox) int64 {
	t.Helper()
	var id int64
	if err := b.s.store.DB.QueryRow(`SELECT id FROM tracks WHERE user_id = ? AND mode = 'driving'`, b.alice.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPocketWidget(t *testing.T) {
	b := newCapBox(t)
	m, body := b.ok(b.aliceTok, "/api/v1/pocket/widget")
	noPlaceOnEarth(t, body)
	today := time.Now().In(b.loc).Format("2006-01-02")
	// home since last night and the café; the hidden clinic does not count
	if m["line"] != "24 km, 2 places today" || m["url"] != "/timeline/"+today || len(items(t, m)) != 0 {
		t.Fatalf("widget: %v", m)
	}
	metrics, _ := m["metrics"].([]any)
	if len(metrics) != 2 || metrics[0].(map[string]any)["value"] != "24 km" || metrics[1].(map[string]any)["value"] != "2" {
		t.Fatalf("metrics: %v", metrics)
	}
	// never where she is now, nor the names of the places
	for _, no := range []string{"Home", "Cafe", "Clinic", "since", "At "} {
		if strings.Contains(body, no) {
			t.Fatalf("the widget says %q: %s", no, body)
		}
	}
	// bob has nothing today: nothing to show
	if m := mustOK(b, b.bobTok, "/api/v1/pocket/widget"); m["line"] != nil || len(items(t, m)) != 0 || m["metrics"] != nil {
		t.Fatalf("bob's widget: %v", m)
	}
}

func TestPocketLinkTokens(t *testing.T) {
	b := newCapBox(t)
	line := `{"request_id":"req-0000000000000001","text":"Cafe Tito"}`
	for _, c := range []struct {
		tok, method, path string
		want              int
	}{
		{"pcx_alice_search", "GET", "/api/v1/pocket/search?q=tito", 200},
		{"pcx_alice_day", "GET", "/api/v1/pocket/day/" + capDay, 200},
		{"pcx_alice_widget", "GET", "/api/v1/pocket/widget", 200},
		{"pcx_alice_widget", "GET", "/api/v1/auth/me", 200},
		// another capability's route
		{"pcx_alice_search", "GET", "/api/v1/pocket/day/" + capDay, 403},
		{"pcx_alice_search", "GET", "/api/v1/pocket/widget", 403},
		{"pcx_alice_day", "GET", "/api/v1/pocket/search?q=tito", 403},
		{"pcx_alice_day", "GET", "/api/v1/pocket/widget", 403},
		{"pcx_alice_widget", "GET", "/api/v1/pocket/search?q=tito", 403},
		{"pcx_alice_widget", "GET", "/api/v1/pocket/day/" + capDay, 403},
		// Movement keeps no quick lines: there is no such route, for anybody
		{"pcx_alice_capture", "POST", "/api/v1/pocket/capture", 404},
		{b.aliceTok, "POST", "/api/v1/pocket/capture", 404},
		{"pcx_alice_capture", "GET", "/api/v1/pocket/widget", 403},
		// the rest of the app, above all positions and routes
		{"pcx_alice_widget", "GET", "/api/v1/now", 403},
		{"pcx_alice_widget", "GET", "/api/v1/where?at=2026-11-20T09:00", 403},
		{"pcx_alice_widget", "GET", "/api/v1/live", 403},
		{"pcx_alice_day", "GET", "/api/v1/day/" + capDay, 403},
		{"pcx_alice_day", "GET", "/api/v1/map?from=" + capDay + "&to=" + capDay, 403},
		{"pcx_alice_day", "GET", "/api/v1/map/points", 403},
		{"pcx_alice_day", "GET", "/api/v1/points", 403},
		{"pcx_alice_day", "GET", "/api/v1/journeys/1/points", 403},
		{"pcx_alice_day", "GET", "/api/v1/summary?date=" + capDay, 403},
		{"pcx_alice_search", "GET", "/api/v1/search?q=tito", 403},
		{"pcx_alice_search", "GET", "/api/v1/places", 403},
		{"pcx_alice_search", "GET", "/api/v1/places/1", 403},
		{"pcx_alice_search", "GET", "/api/v1/export.geojson", 403},
		{"pcx_alice_search", "PUT", "/api/v1/places/1", 403},
		{"pcx_alice_widget", "POST", "/api/v1/visits/bulk", 403},
		// the app's own scopes do not open the new routes
		{"pcx_alice_read", "GET", "/api/v1/pocket/search?q=tito", 403},
		{"pcx_alice_read", "GET", "/api/v1/pocket/day/" + capDay, 403},
		{"pcx_alice_read", "GET", "/api/v1/pocket/widget", 403},
		{"pcx_alice_points", "GET", "/api/v1/pocket/day/" + capDay, 403},
		{"pcx_alice_read", "GET", "/api/v1/search?q=tito", 200},
		{"pcx_alice_read", "GET", "/api/v1/day/" + capDay, 200},
		{"pcx_nobody", "GET", "/api/v1/pocket/widget", 401},
	} {
		body := []byte(nil)
		if c.method == "POST" || c.method == "PUT" {
			body = []byte(line)
		}
		if code, out := b.do(c.tok, c.method, c.path, "application/json", body); code != c.want {
			t.Errorf("%s %s %s → %d, want %d (%s)", c.tok, c.method, c.path, code, c.want, out)
		}
	}
	// the token acts for its person only
	if got := items(t, mustOK(b, "pcx_alice_search", "/api/v1/pocket/search?q=tito")); len(got) != 4 || got[0]["title"] != "Cafe Tito" {
		t.Fatalf("alice through the link: %v", got)
	}
	if d := mustOK(b, "pcx_alice_day", "/api/v1/pocket/day/"+capDay); len(items(t, d)) != 6 || d["summary"] != "7.3 km travelled, 35 min moving, 3 places" {
		t.Fatalf("alice's day through the link: %v", d)
	}
	if w := mustOK(b, "pcx_alice_widget", "/api/v1/pocket/widget"); w["line"] != "24 km, 2 places today" {
		t.Fatalf("alice's widget through the link: %v", w)
	}
	if got := items(t, mustOK(b, "pcx_bob_search", "/api/v1/pocket/search?q=tito")); len(got) != 0 {
		t.Fatalf("bob through the link finds alice's place: %v", got)
	}
	if d := mustOK(b, "pcx_bob_day", "/api/v1/pocket/day/"+capDay); len(items(t, d)) != 0 {
		t.Fatalf("bob's day through the link: %v", d)
	}
	if w := mustOK(b, "pcx_bob_widget", "/api/v1/pocket/widget"); w["line"] != nil {
		t.Fatalf("bob's widget through the link: %v", w)
	}
}

func TestPocketLinkScopes(t *testing.T) {
	req := func(m, p string) *http.Request { return httptest.NewRequest(m, p, nil) }
	for _, c := range []struct {
		scope string
		r     *http.Request
		want  bool
	}{
		{core.ScopeSearch, req("GET", "/api/v1/pocket/search?q=x"), true},
		{core.ScopeDay, req("GET", "/api/v1/pocket/day/2026-01-02"), true},
		{core.ScopeWidget, req("GET", "/api/v1/pocket/widget"), true},
		{core.ScopeWidget, req("GET", "/api/v1/auth/me"), true},
		{core.ScopeCapture, req("POST", "/api/v1/pocket/capture"), false},
		{core.ScopeSearch, req("POST", "/api/v1/pocket/search"), false},
		{core.ScopeDay, req("GET", "/api/v1/pocket/day/today"), false},
		{core.ScopeDay, req("GET", "/api/v1/day/2026-01-02"), false},
		{core.ScopeWidget, req("GET", "/api/v1/now"), false},
		{core.ScopeWidget, req("POST", "/api/v1/pocket/widget"), false},
		{core.ScopeSearch, req("GET", "/api/v1/search"), false},
		{core.ScopeDay, req("GET", "/api/v1/points"), false},
		{"movement:read", req("GET", "/api/v1/pocket/search?q=x"), false},
		{"movement:read", req("GET", "/api/v1/pocket/widget"), false},
		{"movement:points", req("GET", "/api/v1/pocket/day/2026-01-02"), false},
		{"movement:read", req("GET", "/api/v1/day/2026-01-02"), true},
	} {
		if got := linkAllows([]string{c.scope}, c.r); got != c.want {
			t.Errorf("%s %s %s = %v, want %v", c.scope, c.r.Method, c.r.URL.Path, got, c.want)
		}
	}
}

// Movement tells Core what it can do, so Search, Day and Hub find it on their own.
func TestManifestCapabilities(t *testing.T) {
	var mu sync.Mutex
	var manifest map[string]any
	fc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/apps/register" {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&manifest)
			mu.Unlock()
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer fc.Close()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, DBPath: dir + "/t.db", ImportsDir: dir + "/imports", AuthMode: "local", Geocoder: "off", SessionTTL: time.Hour,
		CoreURL: fc.URL, CoreRegistrationKey: "key"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(cfg.DBPath, log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(cfg, st, log, nil)
	if err := s.core.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.registerWithCore(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	m := manifest
	mu.Unlock()
	if m["slug"] != "movement" || m["category"] != "Places" {
		t.Fatalf("manifest: %v %v", m["slug"], m["category"])
	}
	search, _ := m["search"].(map[string]any)
	day, _ := m["day"].(map[string]any)
	widget, _ := m["widget"].(map[string]any)
	if search == nil || day == nil || day["label"] != "Movement" || day["one"] != "place" || day["many"] != "places" || widget == nil || widget["title"] != "Movement today" {
		t.Fatalf("capabilities: %v %v %v", search, day, widget)
	}
	if m["capture"] != nil {
		t.Fatalf("Movement keeps no quick lines: %v", m["capture"])
	}
	bangs, kinds := search["bangs"].([]any), search["kinds"].([]any)
	if len(bangs) != 2 || bangs[0] != "movement" || bangs[1] != "where" || len(kinds) != 2 || kinds[0] != "place" || kinds[1] != "visit" {
		t.Fatalf("search: %v", search)
	}
	offered := map[string]bool{}
	for _, o := range m["offers"].([]any) {
		offered[o.(map[string]any)["scope"].(string)] = true
	}
	for _, sc := range []string{"movement:read", "movement:points", core.ScopeSearch, core.ScopeDay, core.ScopeWidget, core.ScopeMoveExport, core.ScopeMoveImport} {
		if !offered[sc] {
			t.Errorf("%s is not offered", sc)
		}
	}
	if offered[core.ScopeCapture] {
		t.Error("pocket:capture is offered")
	}
}
