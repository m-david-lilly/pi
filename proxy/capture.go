package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// extractText pulls readable text out of an HTML or JSON response so the log
// is searchable. Images and media are never stored.
func extractText(ct string, body []byte, limit int) string {
	if len(body) == 0 || limit <= 0 {
		return ""
	}
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	if strings.Contains(base, "html") || strings.Contains(base, "xml") && !strings.Contains(base, "json") {
		return collapse(stripHTML(body), limit)
	}
	return collapse(string(body), limit)
}

func stripHTML(body []byte) string {
	z := html.NewTokenizer(bytes.NewReader(body))
	var sb strings.Builder
	skip := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return sb.String()
		case html.StartTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "noscript", "template", "svg":
				skip++
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "noscript", "template", "svg":
				if skip > 0 {
					skip--
				}
			}
		case html.TextToken:
			if skip == 0 {
				sb.Write(z.Text())
				sb.WriteByte(' ')
			}
		}
		if sb.Len() > 1<<20 {
			return sb.String()
		}
	}
}

func collapse(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// parseSearch recognises search queries in GET URLs and in POST bodies
// (YouTube's InnerTube search API puts the query in JSON).
func parseSearch(rawurl string, reqBody []byte) *Search {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	q := u.Query()

	if host == "youtube.com" || strings.HasSuffix(host, ".youtube.com") {
		if v := q.Get("search_query"); v != "" {
			return &Search{Engine: "youtube", Query: v}
		}
		if strings.Contains(u.Path, "/youtubei/v1/search") && len(reqBody) > 0 {
			var b struct {
				Query string `json:"query"`
			}
			if json.Unmarshal(reqBody, &b) == nil && b.Query != "" {
				return &Search{Engine: "youtube", Query: b.Query}
			}
		}
	}
	engine := ""
	switch {
	case isGoogleHost(host) && strings.HasPrefix(u.Path, "/search"):
		engine = "google"
	case (host == "bing.com" || strings.HasSuffix(host, ".bing.com")) && strings.HasPrefix(u.Path, "/search"):
		engine = "bing"
	case host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com"):
		engine = "duckduckgo"
	case host == "search.brave.com" && strings.HasPrefix(u.Path, "/search"):
		engine = "brave"
	}
	if engine != "" {
		if v := q.Get("q"); v != "" {
			return &Search{Engine: engine, Query: v}
		}
	}
	return nil
}

// playerResponse is the subset of the InnerTube `player` JSON that carries
// catalogue metadata. Unknown fields (streamingData, captions, ...) are ignored.
type playerResponse struct {
	VideoDetails struct {
		VideoID          string   `json:"videoId"`
		Title            string   `json:"title"`
		Author           string   `json:"author"`
		ChannelID        string   `json:"channelId"`
		LengthSeconds    string   `json:"lengthSeconds"`
		Keywords         []string `json:"keywords"`
		ShortDescription string   `json:"shortDescription"`
		ViewCount        string   `json:"viewCount"`
		IsLiveContent    bool     `json:"isLiveContent"`
		IsLive           bool     `json:"isLive"`
		Thumbnail        struct {
			Thumbnails []thumbnail `json:"thumbnails"`
		} `json:"thumbnail"`
	} `json:"videoDetails"`
	Microformat struct {
		PlayerMicroformatRenderer struct {
			Title struct {
				SimpleText string `json:"simpleText"`
			} `json:"title"`
			Description struct {
				SimpleText string `json:"simpleText"`
			} `json:"description"`
			LengthSeconds     json.Number `json:"lengthSeconds"`
			Category          string      `json:"category"`
			PublishDate       string      `json:"publishDate"`
			UploadDate        string      `json:"uploadDate"`
			ViewCount         json.Number `json:"viewCount"`
			IsFamilySafe      *bool       `json:"isFamilySafe"`
			IsLive            bool        `json:"isLive"`
			OwnerChannelName  string      `json:"ownerChannelName"`
			ExternalChannelID string      `json:"externalChannelId"`
			Thumbnail         struct {
				Thumbnails []thumbnail `json:"thumbnails"`
			} `json:"thumbnail"`
		} `json:"playerMicroformatRenderer"`
	} `json:"microformat"`
}

type thumbnail struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// parseYouTube extracts video metadata from watch URLs, the watch page itself
// and the InnerTube player response. The player response is the richest source
// (category, tags, description, publish date, views, family-safe); the watch
// page's meta tags are the fallback when only the page was seen.
func parseYouTube(rawurl string, reqBody, respBody []byte) *YouTube {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if !isYouTubeHost(host) {
		return nil
	}
	path := u.Path
	yt := &YouTube{VideoID: videoIDFromURL(u, host)}
	isPlayer := strings.Contains(path, "/youtubei/v1/player")
	isWatch := host == "youtu.be" || path == "/watch" ||
		strings.HasPrefix(path, "/shorts/") || strings.HasPrefix(path, "/live/") ||
		strings.HasPrefix(path, "/embed/") || strings.HasPrefix(path, "/v/") ||
		strings.HasPrefix(path, "/e/")
	if isPlayer && yt.VideoID == "" && len(reqBody) > 0 {
		var req struct {
			VideoID string `json:"videoId"`
		}
		if json.Unmarshal(reqBody, &req) == nil {
			yt.VideoID = req.VideoID
		}
	}
	if len(respBody) > 0 && (isPlayer || isWatch) {
		if p := findPlayerResponse(respBody); p != nil {
			applyPlayerResponse(yt, p)
		}
		if isWatch && !isPlayer {
			applyWatchMeta(yt, respBody)
		}
	}
	yt.VideoID = cleanVideoID(yt.VideoID)
	if yt.VideoID == "" {
		return nil
	}
	yt.WatchURL = "https://www.youtube.com/watch?v=" + yt.VideoID
	return yt
}

// youtubeCaptureBytes covers a watch page (~1.4 MB) and an InnerTube player
// response with streamingData. Snippets are still cut to snippet_bytes.
const youtubeCaptureBytes = 4 << 20

// wantsYouTubeMetadata reports whether this request can carry video metadata,
// so the proxy keeps more of the body than the normal max_body_bytes.
func wantsYouTubeMetadata(host, path string) bool {
	if !isYouTubeHost(strings.ToLower(host)) {
		return false
	}
	switch {
	case path == "/watch",
		strings.HasPrefix(path, "/shorts/"),
		strings.HasPrefix(path, "/live/"),
		strings.HasPrefix(path, "/embed/"),
		strings.HasPrefix(path, "/v/"),
		strings.Contains(path, "/youtubei/v1/player"):
		return true
	}
	return false
}

func isYouTubeHost(host string) bool {
	switch host {
	case "youtube.com", "youtu.be", "youtube-nocookie.com", "youtubei.googleapis.com":
		return true
	}
	return strings.HasSuffix(host, ".youtube.com") ||
		strings.HasSuffix(host, ".youtube-nocookie.com")
}

func videoIDFromURL(u *url.URL, host string) string {
	first := func(p string) string {
		p = strings.TrimPrefix(p, "/")
		if i := strings.IndexByte(p, '/'); i >= 0 {
			p = p[:i]
		}
		return p
	}
	switch {
	case host == "youtu.be":
		return first(u.Path)
	case strings.HasPrefix(u.Path, "/shorts/"):
		return first(strings.TrimPrefix(u.Path, "/shorts/"))
	case strings.HasPrefix(u.Path, "/live/"):
		return first(strings.TrimPrefix(u.Path, "/live/"))
	case strings.HasPrefix(u.Path, "/embed/"):
		return first(strings.TrimPrefix(u.Path, "/embed/"))
	case strings.HasPrefix(u.Path, "/v/"):
		return first(strings.TrimPrefix(u.Path, "/v/"))
	case strings.HasPrefix(u.Path, "/e/"):
		return first(strings.TrimPrefix(u.Path, "/e/"))
	}
	return u.Query().Get("v")
}

// findPlayerResponse returns the player object from either an InnerTube player
// response (a JSON document) or a watch page (where it is embedded as
// `ytInitialPlayerResponse = {...}` about 800 KiB into the page).
func findPlayerResponse(body []byte) *playerResponse {
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
		var p playerResponse
		if json.Unmarshal(trimmed, &p) == nil && playerHasData(&p) {
			return &p
		}
	}
	i := bytes.Index(body, []byte("ytInitialPlayerResponse"))
	if i < 0 {
		return nil
	}
	j := bytes.IndexByte(body[i:], '{')
	if j < 0 {
		return nil
	}
	obj := jsonObjectAt(body, i+j)
	if obj == nil {
		return nil
	}
	var p playerResponse
	if json.Unmarshal(obj, &p) == nil && playerHasData(&p) {
		return &p
	}
	return nil
}

func playerHasData(p *playerResponse) bool {
	return p.VideoDetails.VideoID != "" || p.VideoDetails.Title != "" ||
		p.Microformat.PlayerMicroformatRenderer.Category != ""
}

// jsonObjectAt returns the balanced {...} object starting at start. YouTube
// embeds JSON in a script, so a simple brace scan must ignore braces inside
// strings and escapes.
func jsonObjectAt(b []byte, start int) []byte {
	if start < 0 || start >= len(b) || b[start] != '{' {
		return nil
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(b); i++ {
		c := b[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return b[start : i+1]
			}
		}
	}
	return nil
}

func applyPlayerResponse(yt *YouTube, p *playerResponse) {
	vd := &p.VideoDetails
	mf := &p.Microformat.PlayerMicroformatRenderer
	if yt.VideoID == "" {
		yt.VideoID = vd.VideoID
	}
	if yt.Title == "" {
		yt.Title = truncate(vd.Title, 300)
	}
	if yt.Title == "" {
		yt.Title = truncate(mf.Title.SimpleText, 300)
	}
	if yt.Channel == "" {
		yt.Channel = truncate(vd.Author, 200)
	}
	if yt.Channel == "" {
		yt.Channel = truncate(mf.OwnerChannelName, 200)
	}
	if yt.ChannelID == "" {
		yt.ChannelID = vd.ChannelID
	}
	if yt.ChannelID == "" {
		yt.ChannelID = mf.ExternalChannelID
	}
	if yt.Seconds == 0 {
		yt.Seconds = atoi(vd.LengthSeconds)
	}
	if yt.Seconds == 0 {
		yt.Seconds = int(parseInt64(string(mf.LengthSeconds)))
	}
	if yt.Category == "" {
		yt.Category = truncate(mf.Category, 64)
	}
	if len(yt.Tags) == 0 {
		yt.Tags = cleanTags(vd.Keywords)
	}
	if yt.Description == "" {
		yt.Description = truncate(vd.ShortDescription, 4000)
	}
	if yt.Description == "" {
		yt.Description = truncate(mf.Description.SimpleText, 4000)
	}
	if yt.PublishDate == "" {
		yt.PublishDate = truncate(mf.PublishDate, 40)
	}
	if yt.PublishDate == "" {
		yt.PublishDate = truncate(mf.UploadDate, 40)
	}
	if yt.ViewCount == 0 {
		yt.ViewCount = parseInt64(vd.ViewCount)
	}
	if yt.ViewCount == 0 {
		yt.ViewCount = parseInt64(string(mf.ViewCount))
	}
	if yt.FamilySafe == nil && mf.IsFamilySafe != nil {
		v := *mf.IsFamilySafe
		yt.FamilySafe = &v
	}
	if yt.Thumbnail == "" {
		yt.Thumbnail = bestThumbnail(vd.Thumbnail.Thumbnails)
	}
	if yt.Thumbnail == "" {
		yt.Thumbnail = bestThumbnail(mf.Thumbnail.Thumbnails)
	}
	yt.Live = yt.Live || vd.IsLiveContent || vd.IsLive || mf.IsLive
}

// applyWatchMeta fills gaps from the watch page's schema.org meta tags. It
// only sets fields that are still empty, so a player response always wins.
func applyWatchMeta(yt *YouTube, body []byte) {
	z := html.NewTokenizer(bytes.NewReader(body))
	seen := map[string]bool{}
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return
		}
		if tt != html.StartTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) != "meta" || !hasAttr {
			continue
		}
		var key, content string
		for {
			k, v, more := z.TagAttr()
			switch strings.ToLower(string(k)) {
			case "itemprop", "property", "name":
				key = strings.ToLower(string(v))
			case "content":
				content = string(v)
			}
			if !more {
				break
			}
		}
		if key == "" || content == "" || seen[key] {
			continue
		}
		seen[key] = true
		switch key {
		case "name", "og:title": // itemprop="name" is the video title; the channel comes second
			if yt.Title == "" {
				yt.Title = truncate(content, 300)
			}
		case "description", "og:description":
			if yt.Description == "" {
				yt.Description = truncate(content, 4000)
			}
		case "genre":
			if yt.Category == "" {
				yt.Category = truncate(content, 64)
			}
		case "keywords":
			if len(yt.Tags) == 0 {
				yt.Tags = splitTags(content)
			}
		case "datepublished":
			if yt.PublishDate == "" {
				yt.PublishDate = truncate(content, 40)
			}
		case "duration":
			if yt.Seconds == 0 {
				yt.Seconds = parseISODuration(content)
			}
		case "isfamilyfriendly":
			if yt.FamilySafe == nil {
				v := strings.EqualFold(content, "true")
				yt.FamilySafe = &v
			}
		case "identifier":
			if yt.VideoID == "" {
				yt.VideoID = content
			}
		case "og:image":
			if yt.Thumbnail == "" {
				yt.Thumbnail = content
			}
		}
	}
}

func cleanTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		out = append(out, truncate(t, 64))
		if len(out) >= 50 {
			break
		}
	}
	return out
}

func splitTags(s string) []string {
	parts := strings.Split(s, ",")
	return cleanTags(parts)
}

func bestThumbnail(ts []thumbnail) string {
	best := ""
	area := 0
	for _, t := range ts {
		if t.URL == "" {
			continue
		}
		if a := t.Width * t.Height; a >= area {
			best, area = t.URL, a
		}
	}
	return best
}

func parseISODuration(s string) int {
	if !strings.HasPrefix(s, "P") {
		return 0
	}
	total, num := 0, 0
	inTime := false
	for _, r := range s[1:] {
		switch {
		case r == 'T':
			inTime = true
		case r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
		case r == 'H' && inTime:
			total += num * 3600
			num = 0
		case r == 'M' && inTime:
			total += num * 60
			num = 0
		case r == 'S':
			total += num
			num = 0
		case r == 'D':
			total += num * 86400
			num = 0
		}
	}
	return total
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// cleanVideoID keeps only plausible YouTube IDs so a crafted URL cannot inject
// arbitrary text into the database or the CSV.
func cleanVideoID(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.IndexAny(id, "?&#/"); i >= 0 {
		id = id[:i]
	}
	if len(id) > 32 {
		id = id[:32]
	}
	if id == "" {
		return ""
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '-' && r != '_' {
			return ""
		}
	}
	return id
}

// isGoogleHost matches google.com plus country domains (google.co.uk,
// google.de, www.google.com.au) without accepting lookalikes such as
// google.com.evil.net: at most two labels may follow the "google" label.
func isGoogleHost(host string) bool {
	labels := strings.Split(strings.ToLower(host), ".")
	for i, l := range labels {
		if l != "google" {
			continue
		}
		rest := len(labels) - i - 1
		if rest < 1 || rest > 2 {
			return false
		}
		for _, r := range labels[i+1:] {
			if len(r) < 2 || len(r) > 3 {
				return false
			}
			for _, c := range r {
				if c < 'a' || c > 'z' {
					return false
				}
			}
		}
		return true
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0
		}
	}
	return n
}
