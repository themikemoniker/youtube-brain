# YouTube Brain (Apps Script)

The Apps Script version of YouTube Brain. It needs no server, but Apps Script waits at most 60 seconds for Gemini, so long videos fail; the Go CLI in the repo root has no such limit. It summarizes the videos you save to a private "Digest" playlist. Each video gets a Markdown
note in a Drive folder and a row in an index Sheet, then moves to a "Summarized" playlist.

Why it's built this way:

- **Digest playlist, not Watch Later.** The YouTube API refuses to read Watch Later (`watchLaterNotAccessible`).
- **Gemini watches the video by URL**, so there's no captions step and videos without captions work. It only works
  on public videos.
- **One file per video** is easy to search and easy for agents to read. The Sheet is the index and remembers which
  videos are done.

## Setup (about 15 minutes)

1. Get a Gemini API key at https://aistudio.google.com/apikey (free tier).
2. Create a project at https://script.google.com and paste in `apps-script/Code.gs`. Under Project Settings, check "Show
   appsscript.json" and paste that file in too. This turns on the YouTube service.
3. Under Project Settings → Script properties, add `GEMINI_API_KEY`. Add `SEND_DIGEST` = `true` if you want a
   daily email.
4. Run `testOne` and approve the permissions. It logs one summary and writes nothing.
5. Run `setup`. It creates the folder, the Sheet, both playlists and a daily 7am trigger, then logs the Digest
   playlist link.

To move your Watch Later backlog over: open Watch Later, select videos, and choose "Save to playlist" → Digest.

## Limits

- **Gemini's free-tier daily limit:** when Gemini returns 429, the run stops and the rest waits for the next day,
  so a large backlog drains over several days.
- **Long videos:** Apps Script waits at most 60 seconds for Gemini, so a long video can time out.
- **Retrying:** a video that fails (private, unlisted or timed out) gets an `error` row and stays in Digest. Delete
  its row to retry it.
- **Model:** `MODEL` in `Code.gs` is the only place the model name lives.
