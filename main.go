// youtube-brain summarizes each video in a private YouTube "Digest" playlist with Gemini, writes one Markdown note
// per video to a Drive folder, indexes it in a Sheet, and moves the video to a "Summarized" playlist.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/youtube/v3"
)

const usage = `usage: youtube-brain <command>

  auth         sign in to Google and save the token
  setup        create the Drive folder, index Sheet and both playlists (safe to rerun)
  run [--max N] summarize new videos in Digest
  test <url>   summarize one video and print it, writing nothing

Files live in ~/.config/youtube-brain/: client_secret.json (your OAuth client), token-youtube.json and token-drive.json, config.json.
The Gemini key comes from GEMINI_API_KEY or "gemini_api_key" in config.json.`

// Google refuses the YouTube and drive.file scopes in one sign-in, so auth asks twice and saves two tokens.
type grant struct {
	file   string
	scopes []string
}

var (
	ytGrant    = grant{"token-youtube.json", []string{youtube.YoutubeScope}}
	driveGrant = grant{"token-drive.json", []string{drive.DriveFileScope, sheets.SpreadsheetsScope}}
)

type Config struct {
	GeminiAPIKey         string `json:"gemini_api_key,omitempty"`
	FolderID             string `json:"folder_id,omitempty"`
	SheetID              string `json:"sheet_id,omitempty"`
	DigestPlaylistID     string `json:"digest_playlist_id,omitempty"`
	SummarizedPlaylistID string `json:"summarized_playlist_id,omitempty"`
}

var dir = func() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "youtube-brain")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "youtube-brain")
}()

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal(usage)
	}
	var err error
	switch os.Args[1] {
	case "auth":
		err = auth()
	case "setup":
		err = setup()
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		max := fs.Int("max", 0, "summarize at most N videos (0 = all)")
		fs.Parse(os.Args[2:])
		err = run(*max)
	case "test":
		if len(os.Args) != 3 {
			log.Fatal("usage: youtube-brain test <youtube url>")
		}
		err = test(os.Args[2])
	default:
		log.Fatal(usage)
	}
	if err != nil {
		log.Fatal("error: ", err)
	}
}

func loadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func writeJSON(name string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(filepath.Join(dir, name), b, 0o600)
}

func gemini(c Config) (Gemini, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		key = c.GeminiAPIKey
	}
	if key == "" {
		return Gemini{}, fmt.Errorf("no Gemini key: set GEMINI_API_KEY or add \"gemini_api_key\" to %s", filepath.Join(dir, "config.json"))
	}
	// Long videos take minutes; Apps Script's 60-second cap is why it failed on them.
	return Gemini{Key: key, Base: "https://generativelanguage.googleapis.com", Client: &http.Client{Timeout: 5 * time.Minute}}, nil
}

func oauthConfig(g grant) (*oauth2.Config, error) {
	path := filepath.Join(dir, "client_secret.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no OAuth client at %s: download your desktop OAuth client JSON there (see README)", path)
	}
	return google.ConfigFromJSON(b, g.scopes...)
}

func auth() error {
	for i, g := range []grant{ytGrant, driveGrant} {
		fmt.Printf("Sign-in %d of 2 (%s)\n", i+1, strings.Join(g.scopes, ", "))
		if err := signIn(g); err != nil {
			return err
		}
	}
	return nil
}

// signIn runs the desktop OAuth flow with a loopback redirect and saves the token.
func signIn(g grant) error {
	oc, err := oauthConfig(g)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	oc.RedirectURL = fmt.Sprintf("http://%s/", ln.Addr())
	state := randHex()
	verifier := oauth2.GenerateVerifier()
	codes := make(chan string, 1)
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state || q.Get("code") == "" {
			http.Error(w, "sign-in failed: "+q.Get("error"), http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Signed in. You can close this tab.")
		select {
		case codes <- q.Get("code"):
		default:
		}
	}))
	fmt.Println("Open this link to sign in:\n\n" + oc.AuthCodeURL(state, oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier)))
	var code string
	select {
	case code = <-codes:
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for sign-in")
	}
	tok, err := oc.Exchange(context.Background(), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return err
	}
	if err := writeJSON(g.file, tok); err != nil {
		return err
	}
	fmt.Println("Saved", filepath.Join(dir, g.file))
	return nil
}

func randHex() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type services struct {
	ctx context.Context
	yt  *youtube.Service
	dr  *drive.Service
	sh  *sheets.Service
}

func client(ctx context.Context, g grant) (option.ClientOption, error) {
	oc, err := oauthConfig(g)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, g.file))
	if err != nil {
		return nil, errors.New("not signed in: run `youtube-brain auth` first")
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, err
	}
	return option.WithHTTPClient(oc.Client(ctx, &tok)), nil
}

func connect() (*services, error) {
	ctx := context.Background()
	ytOpt, err := client(ctx, ytGrant)
	if err != nil {
		return nil, err
	}
	driveOpt, err := client(ctx, driveGrant)
	if err != nil {
		return nil, err
	}
	s := &services{ctx: ctx}
	if s.yt, err = youtube.NewService(ctx, ytOpt); err != nil {
		return nil, err
	}
	if s.dr, err = drive.NewService(ctx, driveOpt); err != nil {
		return nil, err
	}
	if s.sh, err = sheets.NewService(ctx, driveOpt); err != nil {
		return nil, err
	}
	return s, nil
}

func setup() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	s, err := connect()
	if err != nil {
		return err
	}
	if c.FolderID == "" {
		f, err := s.dr.Files.Create(&drive.File{Name: "YouTube Brain", MimeType: "application/vnd.google-apps.folder"}).Do()
		if err != nil {
			return fmt.Errorf("creating the Drive folder: %w", err)
		}
		c.FolderID = f.Id
		if err := writeJSON("config.json", c); err != nil {
			return err
		}
	}
	if c.SheetID == "" {
		f, err := s.dr.Files.Create(&drive.File{Name: "YouTube Brain index", MimeType: "application/vnd.google-apps.spreadsheet", Parents: []string{c.FolderID}}).Do()
		if err != nil {
			return fmt.Errorf("creating the index Sheet: %w", err)
		}
		if err := (sheetIndex{s, f.Id}).Append(header); err != nil {
			return fmt.Errorf("writing the Sheet header: %w", err)
		}
		c.SheetID = f.Id
		if err := writeJSON("config.json", c); err != nil {
			return err
		}
	}
	for _, p := range []struct {
		id    *string
		title string
	}{{&c.DigestPlaylistID, "Digest"}, {&c.SummarizedPlaylistID, "Summarized"}} {
		if *p.id != "" {
			continue
		}
		pl, err := s.yt.Playlists.Insert([]string{"snippet", "status"}, &youtube.Playlist{
			Snippet: &youtube.PlaylistSnippet{Title: p.title},
			Status:  &youtube.PlaylistStatus{PrivacyStatus: "private"},
		}).Do()
		if err != nil {
			return fmt.Errorf("creating the %s playlist: %w", p.title, err)
		}
		*p.id = pl.Id
		if err := writeJSON("config.json", c); err != nil {
			return err
		}
	}
	fmt.Println("Ready. Save videos to https://www.youtube.com/playlist?list=" + c.DigestPlaylistID)
	return nil
}

func run(max int) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := gemini(c)
	if err != nil {
		return err
	}
	if c.SheetID == "" || c.FolderID == "" || c.DigestPlaylistID == "" || c.SummarizedPlaylistID == "" {
		return errors.New("not set up: run `youtube-brain setup` first")
	}
	s, err := connect()
	if err != nil {
		return err
	}
	n, err := Brain{
		YT: ytPlaylists{s}, Notes: driveNotes{s, c.FolderID}, Index: sheetIndex{s, c.SheetID},
		Summarize: g.Summarize, Digest: c.DigestPlaylistID, Summarized: c.SummarizedPlaylistID, Now: time.Now,
	}.Run(max)
	log.Printf("Summarized %d video(s).", n)
	return err
}

func test(url string) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := gemini(c)
	if err != nil {
		return err
	}
	sum, err := g.Summarize(url)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(b))
	return nil
}

type ytPlaylists struct{ *services }

func (y ytPlaylists) Items(playlistID string) ([]Item, error) {
	var out []Item
	err := y.yt.PlaylistItems.List([]string{"snippet"}).PlaylistId(playlistID).MaxResults(50).Pages(y.ctx, func(r *youtube.PlaylistItemListResponse) error {
		for _, it := range r.Items {
			sn := it.Snippet
			out = append(out, Item{ID: it.Id, VideoID: sn.ResourceId.VideoId, Title: sn.Title, Channel: sn.VideoOwnerChannelTitle, Added: trunc(sn.PublishedAt, 10)})
		}
		return nil
	})
	return out, err
}

// Move copies, then removes: the video is never in neither playlist.
func (y ytPlaylists) Move(it Item, to string) error {
	_, err := y.yt.PlaylistItems.Insert([]string{"snippet"}, &youtube.PlaylistItem{Snippet: &youtube.PlaylistItemSnippet{
		PlaylistId: to, ResourceId: &youtube.ResourceId{Kind: "youtube#video", VideoId: it.VideoID},
	}}).Do()
	if err != nil {
		return err
	}
	return y.yt.PlaylistItems.Delete(it.ID).Do()
}

type driveNotes struct {
	*services
	folder string
}

func (d driveNotes) Write(name, md string) (string, error) {
	f, err := d.dr.Files.Create(&drive.File{Name: name, MimeType: "text/markdown", Parents: []string{d.folder}}).
		Media(strings.NewReader(md)).Fields("webViewLink").Do()
	if err != nil {
		return "", err
	}
	return f.WebViewLink, nil
}

type sheetIndex struct {
	*services
	id string
}

func (s sheetIndex) Seen() (map[string]bool, error) {
	r, err := s.sh.Spreadsheets.Values.Get(s.id, "A2:A").Do()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, row := range r.Values {
		if len(row) > 0 {
			seen[fmt.Sprint(row[0])] = true
		}
	}
	return seen, nil
}

func (s sheetIndex) Append(row []string) error {
	vals := make([]any, len(row))
	for i, v := range row {
		vals[i] = v
	}
	_, err := s.sh.Spreadsheets.Values.Append(s.id, "A1", &sheets.ValueRange{Values: [][]any{vals}}).
		ValueInputOption("RAW").InsertDataOption("INSERT_ROWS").Do()
	return err
}
