package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const model = "gemini-3.5-flash-lite"

var header = []string{"videoId", "title", "channel", "added", "verdict", "tags", "summarized", "note", "url", "error"}

const prompt = `Summarize this video for someone deciding whether to watch it.
summary: 5 bullets, specific (numbers, names, steps), no filler.
keyClaim: the one claim or takeaway the video exists to make.
verdict: "watch" if the visuals or the full argument matter, "skim" if the summary mostly covers it, "skip" if low value.
tags: 2 to 5 lowercase topic tags.`

var schema = json.RawMessage(`{
  "type": "OBJECT",
  "properties": {
    "summary": {"type": "ARRAY", "items": {"type": "STRING"}},
    "keyClaim": {"type": "STRING"},
    "verdict": {"type": "STRING", "enum": ["watch", "skim", "skip"]},
    "tags": {"type": "ARRAY", "items": {"type": "STRING"}}
  },
  "required": ["summary", "keyClaim", "verdict", "tags"]
}`)

// ErrTryLater means Gemini returned 429 (daily quota) or 503 (overloaded): stop the run, the rest waits for the
// next one. Writing error rows instead would skip those videos until their rows were deleted.
var ErrTryLater = errors.New("Gemini says try later")

type Summary struct {
	Summary  []string `json:"summary"`
	KeyClaim string   `json:"keyClaim"`
	Verdict  string   `json:"verdict"`
	Tags     []string `json:"tags"`
}

type Gemini struct {
	Key    string
	Base   string // https://generativelanguage.googleapis.com, swapped in tests
	Client *http.Client
}

func (g Gemini) Summarize(url string) (Summary, error) {
	body, _ := json.Marshal(map[string]any{
		"contents": []any{map[string]any{"parts": []any{
			map[string]any{"file_data": map[string]string{"file_uri": url}},
			map[string]string{"text": prompt},
		}}},
		// low resolution cuts video tokens about 3x, so the free tier covers more videos per day
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   schema,
			"mediaResolution":  "MEDIA_RESOLUTION_LOW",
		},
	})
	req, err := http.NewRequest("POST", g.Base+"/v1beta/models/"+model+":generateContent", bytes.NewReader(body))
	if err != nil {
		return Summary{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.Key)
	res, err := g.Client.Do(req)
	if err != nil {
		return Summary{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	switch res.StatusCode {
	case http.StatusTooManyRequests:
		return Summary{}, fmt.Errorf("%w: 429, quota reached", ErrTryLater)
	case http.StatusServiceUnavailable:
		return Summary{}, fmt.Errorf("%w: 503, overloaded", ErrTryLater)
	}
	if res.StatusCode != http.StatusOK {
		return Summary{}, fmt.Errorf("Gemini %d: %s", res.StatusCode, trunc(string(raw), 200))
	}
	var r struct {
		Candidates []struct {
			Content struct {
				Parts []struct{ Text string }
			}
		}
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Candidates) == 0 || len(r.Candidates[0].Content.Parts) == 0 || r.Candidates[0].Content.Parts[0].Text == "" {
		return Summary{}, fmt.Errorf("Gemini returned no summary: %s", trunc(string(raw), 200))
	}
	var s Summary
	if err := json.Unmarshal([]byte(r.Candidates[0].Content.Parts[0].Text), &s); err != nil {
		return Summary{}, fmt.Errorf("Gemini summary isn't JSON: %w", err)
	}
	return s, nil
}

// Item is one entry in a playlist. ID is the playlist item, VideoID the video.
type Item struct{ ID, VideoID, Title, Channel, Added string }

type Playlists interface {
	Items(playlistID string) ([]Item, error)
	Move(it Item, toPlaylistID string) error // insert into the target, then remove from the source
}

type Notes interface {
	Write(name, markdown string) (url string, err error)
}

type Index interface {
	Seen() (map[string]bool, error)
	Append(row []string) error
}

type Brain struct {
	YT                 Playlists
	Notes              Notes
	Index              Index
	Summarize          func(url string) (Summary, error)
	Digest, Summarized string
	Now                func() time.Time
}

// Run summarizes up to max new Digest videos (0 = all) and returns how many it summarized.
func (b Brain) Run(max int) (int, error) {
	seen, err := b.Index.Seen()
	if err != nil {
		return 0, fmt.Errorf("reading the index sheet: %w", err)
	}
	items, err := b.YT.Items(b.Digest)
	if err != nil {
		return 0, fmt.Errorf("listing Digest: %w", err)
	}
	done, tried := 0, 0
	for _, it := range items {
		if seen[it.VideoID] {
			continue // a row already exists; delete it to retry the video
		}
		if max > 0 && tried >= max {
			break
		}
		tried++
		seen[it.VideoID] = true
		url := "https://www.youtube.com/watch?v=" + it.VideoID
		now := b.Now()
		stamp := now.Format("2006-01-02 15:04:05")
		base := []string{it.VideoID, it.Title, it.Channel, it.Added}

		s, err := b.Summarize(url)
		if errors.Is(err, ErrTryLater) {
			log.Printf("%v; the rest waits for the next run.", err)
			break
		}
		var note string
		if err == nil {
			note, err = b.Notes.Write(NoteName(now, it.Title), NoteMarkdown(it, url, s))
		}
		if err != nil {
			// Private/unlisted videos land here and stay in Digest.
			log.Printf("%s: %v", it.VideoID, err)
			if err := b.Index.Append(append(base, "error", "", stamp, "", url, trunc(err.Error(), 300))); err != nil {
				return done, fmt.Errorf("writing the index sheet: %w", err)
			}
			continue
		}
		if err := b.Index.Append(append(base, s.Verdict, strings.Join(s.Tags, ", "), stamp, note, url, "")); err != nil {
			return done, fmt.Errorf("writing the index sheet: %w", err)
		}
		if err := b.YT.Move(it, b.Summarized); err != nil {
			return done, fmt.Errorf("moving %s to Summarized: %w", it.VideoID, err)
		}
		done++
	}
	return done, nil
}

func NoteMarkdown(it Item, url string, s Summary) string {
	lines := []string{
		"# " + it.Title, "",
		fmt.Sprintf("%s · added %s · %s", it.Channel, it.Added, url), "",
		"**Verdict:** " + s.Verdict, "",
		"**Key claim:** " + s.KeyClaim, "",
	}
	for _, b := range s.Summary {
		lines = append(lines, "- "+b)
	}
	return strings.Join(append(lines, "", "Tags: "+strings.Join(s.Tags, ", "), ""), "\n")
}

var unsafeName = regexp.MustCompile(`[\\/:*?"<>|]`)

func NoteName(day time.Time, title string) string {
	return trunc(unsafeName.ReplaceAllString(day.Format("2006-01-02")+" "+title, "-"), 120) + ".md"
}

// trunc cuts s to n runes.
func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
