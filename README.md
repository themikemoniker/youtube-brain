# YouTube Brain

Save a video to a private "Digest" playlist and the next run summarizes it with Gemini. Each video gets a detailed
Markdown note in a Drive folder and a row in an index Sheet, then moves to a "Summarized" playlist.

A note is meant to replace watching: a TL;DR, the key claim, a timestamped walkthrough, the steps and commands shown,
the tools mentioned, how popular and well received the video is, and how well it fits your own setup.

## Why it's built this way

- **Digest playlist, not Watch Later.** The YouTube API refuses to read Watch Later (`watchLaterNotAccessible`), so
  you save to a playlist the app can read.
- **Gemini watches the video by URL.** There's no captions step, so videos without captions work, and Gemini sees
  the slides and demos as well as the words. It only works on public videos; private and unlisted ones get an `error`
  row.
- **Low media resolution.** It cuts video tokens about 3x, so the free tier covers more videos per day.
- **One file per video** is easy to search and easy for agents to read. The Sheet is the index and remembers which
  videos are done.
- **A Go CLI instead of only Apps Script.** Apps Script waits at most 60 seconds for Gemini, so long videos timed
  out. The CLI waits 5 minutes. The Apps Script version is still in [`apps-script/`](apps-script/) if you'd rather not
  run anything yourself.
- **The `drive.file` scope.** The app can only see the files it creates, not the rest of your Drive.
- **Reception from stats and top comments.** Gemini gets the views, likes, subscriber count, views per day and the 20
  most relevant comments, so it can say whether a video is a hit and what viewers dispute. These cost 3 quota units a
  video; if they fail, the summary goes ahead without them.
- **Fit from a private profile.** `~/.config/youtube-brain/profile.md` describes your machines, tools and taste in
  plain words, and Gemini rates each video great/good/meh/poor against it. It lives outside the repo because it's
  about you. On the free tier, Google may use prompts to improve its models, so keep it to what you'd share.

## How it behaves

- A video that already has a Sheet row is skipped. Delete its row to retry it.
- A 503 (Gemini overloaded) is retried twice, 30s and 60s apart. If it persists, the video is skipped. If a later video
  then succeeds, the 503 was about that one video (some always fail this way), so it gets an `error` row instead of
  blocking the queue. Two 503s in a row, or a 429 (the free-tier daily limit), stop the run, and the rest waits for the
  next run, so a large backlog drains over several days. `run --max N` caps a run at N videos.
- Any other failure, including a timeout, gets an `error` row and the video stays in Digest.
- A video moves by being added to Summarized first, then removed from Digest, so it's never in neither.

## One-time setup

All files live in `~/.config/youtube-brain/`, outside the repo.

1. **Google Cloud project.** At https://console.cloud.google.com create a project. Under APIs & Services → Library,
   enable the YouTube Data API v3, the Google Drive API and the Google Sheets API.
2. **OAuth client.** Under APIs & Services → OAuth consent screen, choose External and add yourself as a test user.
   Then under Credentials → Create credentials → OAuth client ID, choose "Desktop app". Download the JSON to
   `~/.config/youtube-brain/client_secret.json`.
   While the app is in "Testing", Google expires the sign-in after 7 days. Publish the app (you can stay unverified,
   it's only you) to keep the sign-in.
3. **Gemini key.** Get one at https://aistudio.google.com/apikey (free tier). Either export `GEMINI_API_KEY`, or put
   it in `~/.config/youtube-brain/config.json` as `{"gemini_api_key": "..."}` so a scheduled run can find it.
4. **Install and sign in.**

   ```sh
   go install github.com/themikemoniker/youtube-brain@latest
   youtube-brain test 'https://www.youtube.com/watch?v=9hE5-98ZeCg'   # checks the key; writes nothing
   youtube-brain auth    # two Google sign-ins (Google refuses YouTube and Drive in one), saves two tokens
   youtube-brain setup   # creates the folder, Sheet and playlists; prints the Digest link. Rerun after upgrading
                         # to add new Sheet columns.
   ```
5. **Profile (optional).** Write `~/.config/youtube-brain/profile.md`: your machines, tools, languages, interests and
   what "fits" means to you. Without it, the fit rating has nothing to go on.

To move your Watch Later backlog over, open Watch Later, select videos, and choose "Save to playlist" → Digest.

## Scheduling

Examples are in [`deploy/`](deploy/), with install commands in each file's comments:

- **Mac:** `com.themikemoniker.youtube-brain.plist`, a launchd agent that runs at 7am, or on wake if the Mac was
  asleep.
- **Linux:** `youtube-brain.service` and `youtube-brain.timer`, a systemd user timer that runs four times a day,
  with `Persistent=true` so a missed run catches up. Use this on an always-on box so summaries keep coming while your
  laptop is off; the service file has the cross-compile and copy steps.

Run it on one machine only. Two runners could summarize the same video twice.

## Model

`model` in `brain.go` is the only place the model name lives. It is Flash-Lite because the bigger Flash models were
overloaded (503) on the free tier, and Lite costs about a third as much if you ever turn on billing.

The Apps Script version keeps the original short notes; the stats, comments and profile are CLI-only. Its optional
daily email isn't in the CLI either; the Sheet is the digest.
