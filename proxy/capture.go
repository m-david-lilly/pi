package main

import (
	"bytes"
	"encoding/json"
	"net/url"
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

// parseYouTube extracts video metadata from watch URLs and from the InnerTube
// player response (which carries title/channel/length without fetching media).
func parseYouTube(rawurl string, reqBody, respBody []byte) *YouTube {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if !(host == "youtube.com" || strings.HasSuffix(host, ".youtube.com") || host == "youtu.be") {
		return nil
	}
	yt := &YouTube{}
	switch {
	case host == "youtu.be":
		yt.VideoID = strings.Trim(u.Path, "/")
	case strings.HasPrefix(u.Path, "/shorts/"):
		yt.VideoID = strings.TrimPrefix(u.Path, "/shorts/")
	case strings.HasPrefix(u.Path, "/live/"):
		yt.VideoID = strings.TrimPrefix(u.Path, "/live/")
	default:
		yt.VideoID = u.Query().Get("v")
	}

	if strings.Contains(u.Path, "/youtubei/v1/player") && len(respBody) > 0 {
		var p struct {
			VideoDetails struct {
				VideoID       string `json:"videoId"`
				Title         string `json:"title"`
				Author        string `json:"author"`
				LengthSeconds string `json:"lengthSeconds"`
			} `json:"videoDetails"`
		}
		if json.Unmarshal(respBody, &p) == nil {
			if p.VideoDetails.VideoID != "" {
				yt.VideoID = p.VideoDetails.VideoID
			}
			yt.Title = p.VideoDetails.Title
			yt.Channel = p.VideoDetails.Author
			if n := atoi(p.VideoDetails.LengthSeconds); n > 0 {
				yt.Seconds = n
			}
		}
	}
	if yt.VideoID == "" {
		return nil
	}
	if len(yt.VideoID) > 32 {
		yt.VideoID = yt.VideoID[:32]
	}
	return yt
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
