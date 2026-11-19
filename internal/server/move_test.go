package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-movement/internal/config"
	"github.com/abjelosevic88/pocket-homelab-movement/internal/store"
)

type moveBox struct {
	t     *testing.T
	s     *Server
	url   string
	alice *store.User
	bob   *store.User
}

// newMoveBox starts Movement behind a fake Pocket Core whose
// pcx_<who>_<export|import|read> tokens are links acting for alice@ or bob@.
func newMoveBox(t *testing.T) *moveBox {
	t.Helper()
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/auth/introspect" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var in struct{ Token string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		parts := strings.SplitN(strings.TrimPrefix(in.Token, "pcx_"), "_", 2)
		if len(parts) != 2 {
			_, _ = w.Write([]byte(`{"active":false}`))
			return
		}
		email := parts[0] + "@example.com"
		scope := map[string]string{"export": core.ScopeMoveExport, "import": core.ScopeMoveImport, "read": "movement:read"}[parts[1]]
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "kind": "delegated", "scope": scope, "audience": "movement",
			"user": map[string]any{"id": 3, "sub": email, "email": email, "name": parts[0], "role": "member"},
			"app":  map[string]any{"id": 4, "slug": "move", "name": "Pocket Move"}})
	}))
	t.Cleanup(coreSrv.Close)
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, DBPath: dir + "/t.db", ImportsDir: dir + "/imports", AuthMode: "local", Geocoder: "off", SessionTTL: time.Hour,
		RebuildDelay: time.Second, CoreURL: coreSrv.URL, MaxUploadBytes: 64 << 20}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(cfg.DBPath, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(cfg, st, log, nil)
	if err := s.core.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	b := &moveBox{t: t, s: s}
	for _, u := range []*store.User{{Email: "alice@example.com", Name: "alice", Role: "admin"}, {Email: "bob@example.com", Name: "bob", Role: "member"}} {
		if err := st.CreateUser(t.Context(), u); err != nil {
			t.Fatal(err)
		}
	}
	b.alice, _ = st.GetUserByEmail(t.Context(), "alice@example.com")
	b.bob, _ = st.GetUserByEmail(t.Context(), "bob@example.com")
	ts := httptest.NewServer(s.Router())
	t.Cleanup(ts.Close)
	b.url = ts.URL
	return b
}

func (b *moveBox) do(tok, method, path, ctype string, body []byte) (int, string) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.url+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

// upload sends a file the way Pocket Move does and runs the queued import.
func (b *moveBox) upload(tok, name, data string) *store.Import {
	b.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write([]byte(data))
	_ = mw.Close()
	code, body := b.do(tok, "POST", "/api/v1/imports", mw.FormDataContentType(), buf.Bytes())
	if code != http.StatusAccepted {
		b.t.Fatalf("upload %s → %d %s", name, code, body)
	}
	var imp store.Import
	_ = json.Unmarshal([]byte(body), &imp)
	for {
		next, err := b.s.store.NextImport(b.t.Context())
		if err != nil {
			break
		}
		b.s.runImport(b.t.Context(), next)
	}
	got, err := b.s.store.GetImport(b.t.Context(), imp.UserID, imp.ID)
	if err != nil {
		b.t.Fatal(err)
	}
	if got.Status != "done" {
		b.t.Fatalf("import %s: %s %s", name, got.Status, got.Error)
	}
	return got
}

func (b *moveBox) count(table string, u *store.User) int {
	var n int
	_ = b.s.store.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`, u.ID).Scan(&n)
	return n
}

const takeoutRecords = `{
  "locations": [{
    "latitudeE7": 447700000, "longitudeE7": 171900000, "accuracy": 12, "timestamp": "2024-05-01T08:00:00.000Z"
  }, {
    "latitudeE7": 447710000, "longitudeE7": 171910000, "accuracy": 9, "timestampMs": "1714550460000"
  }, {
    "latitudeE7": 447720000, "longitudeE7": 171920000, "timestamp": "2024-05-01T08:02:00Z", "activity": [{"activity": [{"type": "WALKING", "confidence": 80}]}]
  }]
}`

const phoneTimeline = `{
  "semanticSegments": [{
    "startTime": "2024-05-02T10:00:00.000+02:00", "endTime": "2024-05-02T11:30:00.000+02:00",
    "visit": {"topCandidate": {"semanticType": "HOME", "placeLocation": {"latLng": "44.7700000°, 17.1900000°"}}}
  }, {
    "startTime": "2024-05-02T11:30:00.000+02:00", "endTime": "2024-05-02T12:00:00.000+02:00",
    "timelinePath": [{"point": "44.7750000°, 17.1950000°", "time": "2024-05-02T11:40:00.000+02:00"}]
  }],
  "rawSignals": [{"position": {"LatLng": "44.7800000°, 17.2000000°", "accuracyMeters": 5, "timestamp": "2024-05-02T12:10:00.000+02:00"}}]
}`

func TestMoveLinkScopes(t *testing.T) {
	b := newMoveBox(t)
	exp, imp, read := "pcx_bob_export", "pcx_bob_import", "pcx_bob_read"
	for _, c := range []struct {
		tok, method, path string
		want              int
	}{
		{exp, "GET", core.PortablePath, 200},
		{exp, "GET", "/api/v1/export.geojson", 200},
		{exp, "GET", "/api/v1/export.gpx", 200},
		{exp, "GET", "/api/v1/export.csv", 200},
		{exp, "GET", "/api/v1/export?all=1", 403},
		{exp, "POST", "/api/v1/imports", 403},
		{exp, "GET", "/api/v1/imports", 403},
		{exp, "GET", "/api/v1/account/settings", 403},
		{exp, "DELETE", "/api/v1/history", 403},
		{imp, "GET", core.PortablePath, 200},
		{imp, "GET", "/api/v1/export.geojson", 403},
		{imp, "POST", "/api/v1/imports/dawarich", 403},
		{imp, "DELETE", "/api/v1/imports/1", 403},
		{imp, "POST", "/api/v1/rebuild", 403},
		{imp, "GET", "/api/v1/devices", 403},
		{read, "GET", core.PortablePath, 403},
		{read, "GET", "/api/v1/export.geojson", 403},
	} {
		if code, body := b.do(c.tok, c.method, c.path, "application/json", []byte("{}")); code != c.want {
			t.Errorf("%s %s %s → %d, want %d (%s)", c.tok, c.method, c.path, code, c.want, body)
		}
	}
	_, body := b.do(exp, "GET", core.PortablePath, "", nil)
	var p core.Portable
	if err := json.Unmarshal([]byte(body), &p); err != nil || len(p.Exports) != 3 || len(p.Imports) != 3 || p.Imports[1].Kind != core.KindTimeline || p.Imports[1].MaxBytes != 64<<20 {
		t.Fatalf("portable: %s", body)
	}
}

// Google's files come in through a Move link once: a second run of the same
// file adds no points and no visits, and nothing reaches the other person.
func TestMoveImportsGoogleOnce(t *testing.T) {
	b := newMoveBox(t)
	first := b.upload("pcx_alice_import", "Records.json", takeoutRecords)
	if first.Format != "google_records" || first.PointsAdded != 3 {
		t.Fatalf("Records.json: %+v", first)
	}
	if again := b.upload("pcx_alice_import", "Records.json", takeoutRecords); again.PointsAdded != 0 {
		t.Fatalf("Records.json again added %d points", again.PointsAdded)
	}
	tl := b.upload("pcx_alice_import", "Timeline.json", phoneTimeline)
	if tl.Format != "google_timeline" || tl.PointsAdded != 4 || tl.VisitsAdded != 1 {
		t.Fatalf("Timeline.json: %+v", tl)
	}
	// a nameless upload is recognised by what is in it
	if again := b.upload("pcx_alice_import", "upload", phoneTimeline); again.PointsAdded != 0 || again.VisitsAdded != 0 {
		t.Fatalf("Timeline.json again: %+v", again)
	}
	if n := b.count("points", b.alice); n != 7 {
		t.Fatalf("alice has %d points", n)
	}
	if n := b.count("visits", b.alice); n != 1 {
		t.Fatalf("alice has %d visits", n)
	}
	if b.count("points", b.bob) != 0 || b.count("visits", b.bob) != 0 {
		t.Fatal("bob got alice's history")
	}
	if _, body := b.do("pcx_bob_export", "GET", "/api/v1/export.geojson", "", nil); strings.Contains(body, "Point") {
		t.Fatalf("bob's export holds alice's points: %s", body)
	}
}

// Movement's own export goes back in (on another box: here, Bob's account).
func TestMovePocketRoundTrip(t *testing.T) {
	b := newMoveBox(t)
	b.upload("pcx_alice_import", "Records.json", takeoutRecords)
	code, geo := b.do("pcx_alice_export", "GET", "/api/v1/export.geojson", "", nil)
	if code != 200 || strings.Count(geo, `"Feature"`) != 3 {
		t.Fatalf("alice's export: %d %s", code, geo)
	}
	if code, gpx := b.do("pcx_alice_export", "GET", "/api/v1/export.gpx", "", nil); code != 200 || strings.Count(gpx, "<trkpt") != 3 {
		t.Fatalf("alice's gpx: %d %s", code, gpx)
	}
	in := b.upload("pcx_bob_import", "movement.geojson", geo)
	if in.Format != "geojson" || in.PointsAdded != 3 {
		t.Fatalf("pocket import: %+v", in)
	}
	if again := b.upload("pcx_bob_import", "movement.geojson", geo); again.PointsAdded != 0 {
		t.Fatalf("pocket import again added %d", again.PointsAdded)
	}
	_, bobs := b.do("pcx_bob_export", "GET", "/api/v1/export.geojson", "", nil)
	if strings.Count(bobs, `"Feature"`) != 3 || !strings.Contains(bobs, fmt.Sprint(44.77)) {
		t.Fatalf("bob's export: %s", bobs)
	}
}
