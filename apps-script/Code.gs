// YouTube Brain: summarizes each video in the "Digest" playlist with Gemini, writes one Markdown note per video
// to a Drive folder, indexes it in a Sheet, and moves the video to the "Summarized" playlist.
// Watch Later can't be used: the YouTube API refuses it (watchLaterNotAccessible).

const MODEL = "gemini-3.8-flash";
const RUN_BUDGET_MS = 4.5 * 60 * 1000; // Apps Script kills a run at 6 minutes; the rest waits for the next run
const HEADER = [
  "videoId",
  "title",
  "channel",
  "added",
  "verdict",
  "tags",
  "summarized",
  "note",
  "url",
  "error",
];
const PROMPT = `Summarize this video for someone deciding whether to watch it.
summary: 5 bullets, specific (numbers, names, steps), no filler.
keyClaim: the one claim or takeaway the video exists to make.
verdict: "watch" if the visuals or the full argument matter, "skim" if the summary mostly covers it, "skip" if low value.
tags: 2 to 5 lowercase topic tags.`;
const SCHEMA = {
  type: "OBJECT",
  properties: {
    summary: { type: "ARRAY", items: { type: "STRING" } },
    keyClaim: { type: "STRING" },
    verdict: { type: "STRING", enum: ["watch", "skim", "skip"] },
    tags: { type: "ARRAY", items: { type: "STRING" } },
  },
  required: ["summary", "keyClaim", "verdict", "tags"],
};

const props = () => PropertiesService.getScriptProperties();

/** Run once by hand: creates the folder, index sheet and both playlists, then a daily 7am trigger. Safe to rerun. */
function setup() {
  const p = props();
  if (!p.getProperty("GEMINI_API_KEY"))
    throw new Error("Add the GEMINI_API_KEY script property first.");
  if (!p.getProperty("FOLDER_ID"))
    p.setProperty("FOLDER_ID", DriveApp.createFolder("YouTube Brain").getId());
  if (!p.getProperty("SHEET_ID")) {
    const ss = SpreadsheetApp.create("YouTube Brain index");
    ss.getSheets()[0].appendRow(HEADER);
    DriveApp.getFileById(ss.getId()).moveTo(
      DriveApp.getFolderById(p.getProperty("FOLDER_ID")),
    );
    p.setProperty("SHEET_ID", ss.getId());
  }
  for (const [key, title] of [
    ["DIGEST_PLAYLIST_ID", "Digest"],
    ["SUMMARIZED_PLAYLIST_ID", "Summarized"],
  ]) {
    if (!p.getProperty(key)) {
      const pl = YouTube.Playlists.insert(
        { snippet: { title }, status: { privacyStatus: "private" } },
        "snippet,status",
      );
      p.setProperty(key, pl.id);
    }
  }
  ScriptApp.getProjectTriggers()
    .filter((t) => t.getHandlerFunction() === "run")
    .forEach((t) => ScriptApp.deleteTrigger(t));
  ScriptApp.newTrigger("run").timeBased().everyDays(1).atHour(7).create();
  console.log(
    `Ready. Save videos to https://www.youtube.com/playlist?list=${p.getProperty("DIGEST_PLAYLIST_ID")}`,
  );
}

/** The daily job. Also fine to run by hand. */
function run() {
  const started = Date.now();
  const p = props();
  const sheet = SpreadsheetApp.openById(
    p.getProperty("SHEET_ID"),
  ).getSheets()[0];
  const folder = DriveApp.getFolderById(p.getProperty("FOLDER_ID"));
  const n = sheet.getLastRow() - 1;
  const seen = new Set(
    n > 0 ? sheet.getRange(2, 1, n, 1).getValues().flat() : [],
  );
  const fresh = [];

  for (const item of playlistItems(p.getProperty("DIGEST_PLAYLIST_ID"))) {
    if (Date.now() - started > RUN_BUDGET_MS) break;
    const sn = item.snippet;
    const id = sn.resourceId.videoId;
    if (seen.has(id)) continue; // a row already exists; delete it to retry the video
    const url = `https://www.youtube.com/watch?v=${id}`;
    const base = [
      id,
      sn.title,
      sn.videoOwnerChannelTitle || "",
      sn.publishedAt.slice(0, 10),
    ];
    let s;
    try {
      s = summarize(url);
    } catch (e) {
      if (e.quota) {
        console.log("Gemini quota reached; the rest waits for the next run.");
        break;
      }
      // Private/unlisted videos and very long ones (UrlFetchApp waits at most 60s) land here and stay in Digest.
      sheet.appendRow([
        ...base,
        "error",
        "",
        new Date(),
        "",
        url,
        String(e.message).slice(0, 300),
      ]);
      continue;
    }
    const note = writeNote(folder, sn, url, s);
    sheet.appendRow([
      ...base,
      s.verdict,
      s.tags.join(", "),
      new Date(),
      note.getUrl(),
      url,
      "",
    ]);
    move(item, p.getProperty("SUMMARIZED_PLAYLIST_ID"));
    fresh.push({ title: sn.title, url, ...s });
  }
  if (fresh.length && p.getProperty("SEND_DIGEST") === "true")
    emailDigest(fresh);
  console.log(`Summarized ${fresh.length} video(s).`);
}

function playlistItems(playlistId) {
  const items = [];
  let pageToken;
  do {
    const r = YouTube.PlaylistItems.list("snippet", {
      playlistId,
      maxResults: 50,
      pageToken,
    });
    items.push(...(r.items || []));
    pageToken = r.nextPageToken;
  } while (pageToken);
  return items;
}

function summarize(url) {
  const res = UrlFetchApp.fetch(
    `https://generativelanguage.googleapis.com/v1beta/models/${MODEL}:generateContent`,
    {
      method: "post",
      contentType: "application/json",
      muteHttpExceptions: true,
      headers: { "x-goog-api-key": props().getProperty("GEMINI_API_KEY") },
      payload: JSON.stringify({
        contents: [
          { parts: [{ file_data: { file_uri: url } }, { text: PROMPT }] },
        ],
        // low resolution cuts video tokens about 3x, so the free tier covers more videos per day
        generationConfig: {
          responseMimeType: "application/json",
          responseSchema: SCHEMA,
          mediaResolution: "MEDIA_RESOLUTION_LOW",
        },
      }),
    },
  );
  const code = res.getResponseCode();
  const body = res.getContentText();
  if (code === 429)
    throw Object.assign(new Error("Gemini quota reached"), { quota: true });
  if (code !== 200) throw new Error(`Gemini ${code}: ${body.slice(0, 200)}`);
  const text = JSON.parse(body).candidates?.[0]?.content?.parts?.[0]?.text;
  if (!text)
    throw new Error(`Gemini returned no summary: ${body.slice(0, 200)}`);
  return JSON.parse(text);
}

function writeNote(folder, sn, url, s) {
  const md = [
    `# ${sn.title}`,
    "",
    `${sn.videoOwnerChannelTitle || ""} · added ${sn.publishedAt.slice(0, 10)} · ${url}`,
    "",
    `**Verdict:** ${s.verdict}`,
    "",
    `**Key claim:** ${s.keyClaim}`,
    "",
    ...s.summary.map((b) => `- ${b}`),
    "",
    `Tags: ${s.tags.join(", ")}`,
    "",
  ].join("\n");
  const day = Utilities.formatDate(
    new Date(),
    Session.getScriptTimeZone(),
    "yyyy-MM-dd",
  );
  const name =
    `${day} ${sn.title}`.replace(/[\\/:*?"<>|]/g, "-").slice(0, 120) + ".md";
  return folder.createFile(name, md, MimeType.PLAIN_TEXT);
}

/** Copy, then remove: the video is never in neither playlist. */
function move(item, toPlaylistId) {
  YouTube.PlaylistItems.insert(
    {
      snippet: {
        playlistId: toPlaylistId,
        resourceId: item.snippet.resourceId,
      },
    },
    "snippet",
  );
  YouTube.PlaylistItems.remove(item.id);
}

function emailDigest(rows) {
  const body = rows
    .map(
      (r) =>
        `${r.verdict.toUpperCase()}  ${r.title}\n${r.keyClaim}\n${r.url}\n`,
    )
    .join("\n");
  MailApp.sendEmail(
    Session.getEffectiveUser().getEmail(),
    `YouTube Brain: ${rows.length} new`,
    body,
  );
}

/** Check the Gemini key and prompt on one public video. Writes nothing. */
function testOne() {
  console.log(
    JSON.stringify(
      summarize("https://www.youtube.com/watch?v=9hE5-98ZeCg"),
      null,
      2,
    ),
  );
}
