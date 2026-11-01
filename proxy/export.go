package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// exportCmd is the machine-readable companion to query. The dashboard's CSV
// endpoint uses the TSV form (one row per video, local date/weekday/hour) and
// the analytics drill-down uses the JSON form.
func exportCmd(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := fs.String("db", "/var/lib/kidproxy/events.db", "database path")
	kind := fs.String("kind", "youtube", "youtube")
	since := fs.String("since", "7d", "lookback window, e.g. 24h or 7d")
	format := fs.String("format", "tsv", "tsv|json")
	configPath := fs.String("config", "", "optional config file (uses alert.timezone)")
	tz := fs.String("tz", "", "IANA timezone for local date/hour, e.g. America/Phoenix")
	limit := fs.Int("limit", 5000, "max rows")
	_ = fs.Parse(args)

	if *kind != "youtube" {
		fail(fmt.Errorf("export: unsupported --kind %q (use youtube)", *kind))
	}
	dur, err := parseSince(*since)
	if err != nil {
		fail(err)
	}
	if *limit <= 0 || *limit > 50000 {
		*limit = 5000
	}

	loc := time.Local
	if *configPath != "" {
		if cfg, err := loadConfig(*configPath); err == nil && cfg.loc != nil {
			loc = cfg.loc
		}
	}
	if *tz != "" {
		l, err := time.LoadLocation(*tz)
		if err != nil {
			fail(fmt.Errorf("--tz: %w", err))
		}
		loc = l
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", *dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fail(err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT ts,device,video_id,title,channel,channel_id,seconds,
		category,tags,description,publish_date,view_count,family_safe,thumbnail,
		watch_url,live,query
		FROM youtube WHERE ts>=? ORDER BY ts ASC LIMIT ?`,
		time.Now().Add(-dur).Unix(), *limit)
	if err != nil {
		fail(err)
	}
	defer rows.Close()

	videos := make([]exportVideo, 0, 64)
	for rows.Next() {
		var (
			ts                                   int64
			device, videoID, title, channel      sql.NullString
			channelID, category, tags, desc      sql.NullString
			publishDate, thumbnail, watchURL     sql.NullString
			query                                sql.NullString
			seconds, viewCount, familySafe, live sql.NullInt64
		)
		if err := rows.Scan(&ts, &device, &videoID, &title, &channel, &channelID,
			&seconds, &category, &tags, &desc, &publishDate, &viewCount,
			&familySafe, &thumbnail, &watchURL, &live, &query); err != nil {
			fail(err)
		}
		t := time.Unix(ts, 0).In(loc)
		url := watchURL.String
		if url == "" && videoID.String != "" {
			url = "https://www.youtube.com/watch?v=" + videoID.String
		}
		v := exportVideo{
			TS:          time.Unix(ts, 0).UTC().Format(time.RFC3339),
			Date:        t.Format("2006-01-02"),
			Weekday:     t.Format("Mon"),
			Hour:        t.Hour(),
			Device:      device.String,
			VideoID:     videoID.String,
			Title:       title.String,
			Channel:     channel.String,
			ChannelID:   channelID.String,
			Category:    category.String,
			Description: desc.String,
			Seconds:     int(seconds.Int64),
			PublishDate: publishDate.String,
			ViewCount:   viewCount.Int64,
			Thumbnail:   thumbnail.String,
			Live:        live.Int64 != 0,
			URL:         url,
			Query:       query.String,
		}
		if tags.String != "" {
			_ = json.Unmarshal([]byte(tags.String), &v.Tags)
		}
		if familySafe.Valid {
			b := familySafe.Int64 != 0
			v.FamilySafe = &b
		}
		videos = append(videos, v)
	}
	if err := rows.Err(); err != nil {
		fail(err)
	}

	if strings.EqualFold(*format, "json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		_ = enc.Encode(videos)
		return
	}
	if !strings.EqualFold(*format, "tsv") {
		fail(fmt.Errorf("export: unsupported --format %q (use tsv or json)", *format))
	}
	writeExportTSV(os.Stdout, videos)
}

func writeExportTSV(w io.Writer, videos []exportVideo) {
	fmt.Fprintln(w, "date\tweekday\thour\tdevice\tvideo_id\ttitle\tchannel\tcategory\ttags\tduration_seconds\tpublish_date\tview_count\tfamily_safe\turl")
	for _, v := range videos {
		family := ""
		if v.FamilySafe != nil {
			family = fmt.Sprint(*v.FamilySafe)
		}
		tags := ""
		if len(v.Tags) > 0 {
			if b, err := json.Marshal(v.Tags); err == nil {
				tags = string(b)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%02d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\n",
			v.Date, v.Weekday, v.Hour, tsv(v.Device), tsv(v.VideoID), tsv(v.Title),
			tsv(v.Channel), tsv(v.Category), tsv(tags), v.Seconds,
			tsv(v.PublishDate), v.ViewCount, family, tsv(v.URL))
	}
}

type exportVideo struct {
	TS          string   `json:"ts"`
	Date        string   `json:"date"`
	Weekday     string   `json:"weekday"`
	Hour        int      `json:"hour"`
	Device      string   `json:"device"`
	VideoID     string   `json:"video_id"`
	Title       string   `json:"title"`
	Channel     string   `json:"channel,omitempty"`
	ChannelID   string   `json:"channel_id,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description,omitempty"`
	Seconds     int      `json:"seconds"`
	PublishDate string   `json:"publish_date,omitempty"`
	ViewCount   int64    `json:"view_count,omitempty"`
	FamilySafe  *bool    `json:"family_safe,omitempty"`
	Thumbnail   string   `json:"thumbnail,omitempty"`
	Live        bool     `json:"live"`
	URL         string   `json:"url"`
	Query       string   `json:"query,omitempty"`
}

// tsv keeps a value on one line and in one column.
func tsv(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
