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
		if p[0].FileData.FileURI != "https://www.youtube.com/watch?v=abc" || p[1].Text != prompt ||
			gc.MediaResolution != "MEDIA_RESOLUTION_LOW" || gc.ResponseMimeType != "application/json" || len(gc.ResponseSchema.Required) != 4 {
			t.Errorf("bad request: %s", b)
		}
		inner, _ := json.Marshal(Summary{Summary: []string{"a", "b"}, KeyClaim: "k", Verdict: "skim", Tags: []string{"go"}})
		json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]string{"text": string(inner)}}}}}})
	}))
	defer srv.Close()

	s, err := Gemini{Key: "k", Base: srv.URL, Client: srv.Client()}.Summarize("https://www.youtube.com/watch?v=abc")
	if err != nil || s.Verdict != "skim" || len(s.Summary) != 2 || s.KeyClaim != "k" {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestGeminiErrors(t *testing.T) {
	for code, want := range map[int]string{429: "quota", 500: "Gemini 500", 200: "no summary"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"candidates":[]}`))
		}))
		_, err := Gemini{Base: srv.URL, Client: srv.Client()}.Summarize("u")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), want) || (code == 429) != errors.Is(err, ErrQuota) {
			t.Errorf("%d: got %v", code, err)
		}
	}
}

func TestNote(t *testing.T) {
	day := time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)
	if got := NoteName(day, `a/b:c*d?"e<f>g|h\i`); got != "2026-11-24 a-b-c-d--e-f-g-h-i.md" {
		t.Errorf("name %q", got)
	}
	if got := NoteName(day, strings.Repeat("é", 200)); len([]rune(got)) != 123 {
		t.Errorf("long name is %d runes", len([]rune(got)))
	}
	md := NoteMarkdown(Item{Title: "T", Channel: "C", Added: "2026-11-20"}, "U", Summary{Summary: []string{"one", "two"}, KeyClaim: "K", Verdict: "watch", Tags: []string{"x", "y"}})
	want := "# T\n\nC · added 2026-11-20 · U\n\n**Verdict:** watch\n\n**Key claim:** K\n\n- one\n- two\n\nTags: x, y\n"
	if md != want {
		t.Errorf("markdown:\n%s", md)
	}
}

type fakeYT struct {
	items []Item
	moved []string
}

func (f *fakeYT) Items(string) ([]Item, error) { return f.items, nil }
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
	yt := &fakeYT{items: []Item{{ID: "p1", VideoID: "done"}, {ID: "p2", VideoID: "ok"}, {ID: "p3", VideoID: "bad"}, {ID: "p4", VideoID: "quota"}, {ID: "p5", VideoID: "later"}}}
	notes, idx := &fakeNotes{}, &fakeIndex{seen: map[string]bool{"done": true}}
	var asked []string
	b := Brain{
		YT: yt, Notes: notes, Index: idx, Digest: "D", Summarized: "S",
		Now: func() time.Time { return time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC) },
		Summarize: func(url string) (Summary, error) {
			asked = append(asked, url)
			switch {
			case strings.HasSuffix(url, "bad"):
				return Summary{}, errors.New("private video")
			case strings.HasSuffix(url, "quota"):
				return Summary{}, ErrQuota
			}
			return Summary{Verdict: "skim", Tags: []string{"a", "b"}}, nil
		},
	}
	n, err := b.Run(0)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(asked) != 3 || strings.HasSuffix(asked[0], "done") {
		t.Errorf("asked %v: should skip done, stop at quota", asked)
	}
	if len(yt.moved) != 1 || yt.moved[0] != "ok>S" {
		t.Errorf("moved %v: only ok should move", yt.moved)
	}
	if len(idx.rows) != 2 || idx.rows[0][4] != "skim" || idx.rows[0][5] != "a, b" || idx.rows[0][7] != "note://2026-11-24 .md" ||
		idx.rows[1][4] != "error" || idx.rows[1][9] != "private video" {
		t.Errorf("rows %q", idx.rows)
	}

	// --max counts attempts, not successes.
	asked, idx.rows, idx.seen = nil, nil, map[string]bool{}
	if _, err := b.Run(2); err != nil || len(asked) != 2 {
		t.Errorf("max 2 asked %v, err %v", asked, err)
	}
}
