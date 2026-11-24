package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGeminiRequestAndParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/"+model+":generateContent" || r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("path %s key %q", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		var req struct {
			Contents []struct {
				Parts []struct {
					FileData *struct {
						FileURI string `json:"file_uri"`
					} `json:"file_data"`
					Text string
				}
			}
			GenerationConfig struct {
				ResponseMimeType string
				MediaResolution  string
				ResponseSchema   struct{ Required []string }
			}
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		p := req.Contents[0].Parts
		gc := req.GenerationConfig
		if len(p) != 3 || p[0].FileData.FileURI != "https://www.youtube.com/watch?v=abc" || p[1].Text != prompt || p[2].Text != "CTX" ||
			gc.MediaResolution != "MEDIA_RESOLUTION_LOW" || gc.ResponseMimeType != "application/json" ||
			!strings.Contains(strings.Join(gc.ResponseSchema.Required, ","), "fit") {
			t.Errorf("bad request: %s", b)
		}
		inner := `{"tldr":"t","summary":["a","b"],"keyClaim":"k","sections":[{"start":"0:00","title":"Intro","points":["p"]}],
			"steps":[],"mentioned":[{"name":"Go","note":"used"}],"reception":"liked","fit":{"rating":"good","why":"w"},"verdict":"skim","tags":["go"]}`
		json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]string{"text": inner}}}}}})
	}))
	defer srv.Close()

	s, err := Gemini{Key: "k", Base: srv.URL, Client: srv.Client()}.Summarize("https://www.youtube.com/watch?v=abc", "CTX")
	if err != nil || s.Verdict != "skim" || s.Fit.Rating != "good" || s.Sections[0].Title != "Intro" || s.Mentioned[0].Name != "Go" || s.TLDR != "t" {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestGeminiErrors(t *testing.T) {
	for code, want := range map[int]string{429: "quota", 503: "overloaded", 500: "Gemini 500", 200: "no summary"} {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(code)
			w.Write([]byte(`{"candidates":[]}`))
		}))
		_, err := Gemini{Base: srv.URL, Client: srv.Client()}.Summarize("u", "")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) || (code == 429 || code == 503) != errors.Is(err, ErrTryLater) {
			t.Errorf("%d: got %v", code, err)
		}
		if wantCalls := map[bool]int{true: 3, false: 1}[code == 503]; calls != wantCalls {
			t.Errorf("%d: %d calls, want %d (only 503 is retried)", code, calls, wantCalls)
		}
	}
}

func TestDurationAndNumbers(t *testing.T) {
	for in, want := range map[string]string{"PT18M42S": "18:42", "PT1H2M3S": "1:02:03", "PT45S": "0:45", "P0D": "0:00", "junk": "0:00"} {
		if got := clock(ParseISODuration(in)); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	for n, want := range map[float64]string{950: "950", 12345: "12.3k", 4100000: "4.1M", 2000: "2k"} {
		if got := human(n); got != want {
			t.Errorf("%v: %s, want %s", n, got, want)
		}
	}
}

var now = time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)

func TestNote(t *testing.T) {
	if got := NoteName(now, `a/b:c*d?"e<f>g|h\i`); got != "2026-11-24 a-b-c-d--e-f-g-h-i.md" {
		t.Errorf("name %q", got)
	}
	if got := NoteName(now, strings.Repeat("é", 200)); len([]rune(got)) != 123 {
		t.Errorf("long name is %d runes", len([]rune(got)))
	}
	d := Details{Views: 100000, Likes: 5200, Comments: 310, Subscribers: 450000, Duration: 1122 * time.Second, Uploaded: now.AddDate(0, 0, -10)}
	s := Summary{TLDR: "Short.", Summary: []string{"one"}, KeyClaim: "K", Sections: []Section{{Start: "0:00", Title: "Intro", Points: []string{"p1", "p2"}}},
		Mentioned: []Mention{{Name: "Go", Note: "used"}}, Reception: "Well liked.", Fit: Fit{Rating: "good", Why: "Fits."}, Verdict: "watch", Tags: []string{"x", "y"}}
	md := NoteMarkdown(Item{Title: "T", Channel: "C", Added: "2026-11-20"}, "U", d, s, now)
	want := `# T

C · 18:42 · uploaded 2026-11-14 · saved 2026-11-20 · U

**Verdict:** watch · **Fit:** good

> Short.

## Key claim

K

## Summary

- one

## Walkthrough

### 0:00 Intro

- p1
- p2

## Mentioned

- **Go**: used

## Reception

100k views · 10k/day · 5.2k likes · 310 comments · 450k subscribers · uploaded 2026-11-14

Well liked.

## Fit for my setup

**good**: Fits.

Tags: x, y
`
	if md != want {
		t.Errorf("markdown:\n%s", md) // no Steps section: it was empty
	}
}

func TestContext(t *testing.T) {
	c := Context(Details{Views: 10, TopComments: []string{"great\nvideo"}}, "  I use Go.\n", now)
	if !strings.Contains(c, "Stats: 10 views") || !strings.Contains(c, "- great video") || !strings.Contains(c, "I use Go.") {
		t.Errorf("context:\n%s", c)
	}
	if Context(Details{}, "", now) != "" {
		t.Error("empty details and profile should give no context")
	}
}

type fakeYT struct {
	items []Item
	moved []string
}

func (f *fakeYT) Items(string) ([]Item, error) { return f.items, nil }
func (f *fakeYT) Details(id string) (Details, error) {
	if id == "nostats" {
		return Details{}, errors.New("api down")
	}
	return Details{Views: 1234, Duration: 90 * time.Second}, nil
}
func (f *fakeYT) Move(it Item, to string) error {
	f.moved = append(f.moved, it.VideoID+">"+to)
	return nil
}

type fakeNotes struct{ names []string }

func (f *fakeNotes) Write(name, md string) (string, error) {
	f.names = append(f.names, name)
	return "note://" + name, nil
}

type fakeIndex struct {
	seen map[string]bool
	rows [][]string
}

func (f *fakeIndex) Seen() (map[string]bool, error) { return f.seen, nil }
func (f *fakeIndex) Append(r []string) error        { f.rows = append(f.rows, r); return nil }

func TestRun(t *testing.T) {
	yt := &fakeYT{items: []Item{{ID: "p1", VideoID: "done"}, {ID: "p2", VideoID: "ok"}, {ID: "p3", VideoID: "nostats"}, {ID: "p4", VideoID: "bad"}, {ID: "p5", VideoID: "quota"}, {ID: "p6", VideoID: "later"}}}
	notes, idx := &fakeNotes{}, &fakeIndex{seen: map[string]bool{"done": true}}
	var asked, contexts []string
	b := Brain{
		YT: yt, Notes: notes, Index: idx, Digest: "D", Summarized: "S", Profile: "PROFILE",
		Now: func() time.Time { return now },
		Summarize: func(url, ctx string) (Summary, error) {
			asked, contexts = append(asked, url), append(contexts, ctx)
			switch {
			case strings.HasSuffix(url, "bad"):
				return Summary{}, errors.New("private video")
			case strings.HasSuffix(url, "quota"):
				return Summary{}, ErrTryLater
			}
			return Summary{Verdict: "skim", Tags: []string{"a", "b"}, Fit: Fit{Rating: "great"}}, nil
		},
	}
	n, err := b.Run(0)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(asked) != 4 || strings.HasSuffix(asked[0], "done") {
		t.Errorf("asked %v: should skip done, stop at quota", asked)
	}
	if !strings.Contains(contexts[0], "1.2k views") || !strings.Contains(contexts[0], "PROFILE") || strings.Contains(contexts[1], "views") {
		t.Errorf("contexts %q", contexts)
	}
	if strings.Join(yt.moved, ",") != "ok>S,nostats>S" {
		t.Errorf("moved %v: ok and nostats should move", yt.moved)
	}
	ok, errRow := idx.rows[0], idx.rows[2]
	if len(idx.rows) != 3 || len(ok) != len(header) || ok[4] != "skim" || ok[5] != "a, b" || ok[7] != "note://2026-11-24 .md" ||
		ok[10] != "great" || ok[11] != "1234" || ok[12] != "1:30" || errRow[4] != "error" || errRow[9] != "private video" || len(errRow) != len(header) {
		t.Errorf("rows %q", idx.rows)
	}

	// --max counts attempts, not successes.
	asked, idx.rows, idx.seen = nil, nil, map[string]bool{}
	if _, err := b.Run(2); err != nil || len(asked) != 2 {
		t.Errorf("max 2 asked %v, err %v", asked, err)
	}
}

func TestRunSkipsAVideoThatAlways503s(t *testing.T) {
	yt := &fakeYT{items: []Item{{VideoID: "poison"}, {VideoID: "good"}, {VideoID: "busy1"}, {VideoID: "busy2"}, {VideoID: "never"}}}
	idx := &fakeIndex{seen: map[string]bool{}}
	var asked []string
	n, err := Brain{
		YT: yt, Notes: &fakeNotes{}, Index: idx, Digest: "D", Summarized: "S", Now: func() time.Time { return now },
		Summarize: func(url, ctx string) (Summary, error) {
			asked = append(asked, url[strings.LastIndex(url, "=")+1:])
			if strings.HasSuffix(url, "good") {
				return Summary{Verdict: "skim"}, nil
			}
			return Summary{}, errOverloaded
		},
	}.Run(0)
	if err != nil || n != 1 || strings.Join(asked, ",") != "poison,good,busy1,busy2" {
		t.Fatalf("n=%d err=%v asked=%v: poison skipped, good done, two 503s in a row stop", n, err, asked)
	}
	if len(idx.rows) != 2 || idx.rows[0][0] != "poison" || idx.rows[0][4] != "error" || idx.rows[1][0] != "good" || idx.rows[1][4] != "skim" {
		t.Errorf("rows %q: poison gets an error row once good succeeds; busy1 gets none", idx.rows)
	}
	if strings.Join(yt.moved, ",") != "good>S" {
		t.Errorf("moved %v", yt.moved)
	}
}
