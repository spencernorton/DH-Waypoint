// Package stremio provides a standalone Stremio Addon v1 HTTP sidecar backed by
// DH-Waypoint's Torznab API, importing only the Go standard library.
package stremio

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string
	TorznabURL string
	TorznabKey string
	AddonName  string
	MaxStreams int
}

type Manifest struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Resources   []string `json:"resources"`
	Types       []string `json:"types"`
	IDPrefixes  []string `json:"idPrefixes"`
}

type BehaviorHints struct {
	BingeGroup string `json:"bingeGroup,omitempty"`
}

type Stream struct {
	Name          string         `json:"name"`
	Title         string         `json:"title"`
	InfoHash      string         `json:"infoHash"`
	BehaviorHints *BehaviorHints `json:"behaviorHints,omitempty"`
}

type StreamResponse struct {
	Streams []Stream `json:"streams"`
}

type rssFeed struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title string    `xml:"title"`
	Size  int64     `xml:"size"`
	Attrs []rssAttr `xml:"attr"`
}

type rssAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type Service struct {
	cfg    Config
	client *http.Client
}

func NewService(cfg Config) *Service {
	if cfg.AddonName == "" {
		cfg.AddonName = "DH-Waypoint"
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 25
	}
	return &Service{
		cfg:    cfg,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manifest.json", s.handleManifest)
	mux.HandleFunc("GET /stream/{mediaType}/{rawID}", s.handleStream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return withCORS(mux)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) handleManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Manifest{
		ID:          "org.dhwaypoint.stremio",
		Version:     "1.0.0",
		Name:        s.cfg.AddonName,
		Description: "Verified torrent streams served from DH-Waypoint Torznab",
		Resources:   []string{"stream"},
		Types:       []string{"movie", "series"},
		IDPrefixes:  []string{"tt"},
	})
}

func (s *Service) handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	mediaType := r.PathValue("mediaType")
	rawID := strings.TrimSuffix(r.PathValue("rawID"), ".json")
	imdbID, season, episode, ok := parseStremioID(mediaType, rawID)
	if !ok {
		_ = json.NewEncoder(w).Encode(StreamResponse{Streams: []Stream{}})
		return
	}
	streams, err := s.queryTorznab(r.Context(), mediaType, imdbID, season, episode)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(StreamResponse{Streams: []Stream{}})
		return
	}
	_ = json.NewEncoder(w).Encode(StreamResponse{Streams: streams})
}

func parseStremioID(mediaType, raw string) (imdbID string, season, episode int, ok bool) {
	parts := strings.Split(raw, ":")
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "tt") || len(parts[0]) < 4 {
		return "", 0, 0, false
	}
	for _, ch := range parts[0][2:] {
		if ch < '0' || ch > '9' {
			return "", 0, 0, false
		}
	}
	switch mediaType {
	case "movie":
		if len(parts) != 1 {
			return "", 0, 0, false
		}
		return parts[0], 0, 0, true
	case "series":
		if len(parts) != 3 {
			return "", 0, 0, false
		}
		s, errS := strconv.Atoi(parts[1])
		e, errE := strconv.Atoi(parts[2])
		if errS != nil || errE != nil || s < 0 || e < 0 {
			return "", 0, 0, false
		}
		return parts[0], s, e, true
	default:
		return "", 0, 0, false
	}
}

func (s *Service) queryTorznab(ctx context.Context, mediaType, imdbID string, season, episode int) ([]Stream, error) {
	u, err := url.Parse(strings.TrimRight(s.cfg.TorznabURL, "/"))
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(u.Path, "/torznab/api") && !strings.HasSuffix(u.Path, "/api") {
		u.Path = strings.TrimRight(u.Path, "/") + "/torznab/api"
	}
	q := u.Query()
	q.Set("imdbid", imdbID)
	q.Set("limit", strconv.Itoa(s.cfg.MaxStreams))
	if mediaType == "series" {
		q.Set("t", "tvsearch")
		q.Set("season", strconv.Itoa(season))
		q.Set("ep", strconv.Itoa(episode))
	} else {
		q.Set("t", "movie")
	}
	if s.cfg.TorznabKey != "" {
		q.Set("apikey", s.cfg.TorznabKey)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("torznab status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var feed rssFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, err
	}
	out := make([]Stream, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		var hash, resolution, video, seeders string
		for _, a := range item.Attrs {
			switch a.Name {
			case "infohash":
				hash = strings.ToLower(strings.TrimSpace(a.Value))
			case "resolution":
				resolution = a.Value
			case "video":
				video = a.Value
			case "seeders":
				seeders = a.Value
			}
		}
		if len(hash) != 40 {
			continue
		}
		badge := s.cfg.AddonName
		if resolution != "" {
			badge += " " + resolution
		}
		var meta []string
		if item.Size > 0 {
			meta = append(meta, fmt.Sprintf("%.2f GiB", float64(item.Size)/(1024*1024*1024)))
		}
		if seeders != "" {
			meta = append(meta, "S:"+seeders)
		}
		if video != "" {
			meta = append(meta, video)
		}
		title := item.Title
		if len(meta) > 0 {
			title += "\n" + strings.Join(meta, " | ")
		}
		var hints *BehaviorHints
		if resolution != "" || video != "" {
			hints = &BehaviorHints{BingeGroup: "dhw-" + strings.ToLower(resolution+"-"+video)}
		}
		out = append(toutSafe(out), Stream{
			Name:          badge,
			Title:         title,
			InfoHash:      hash,
			BehaviorHints: hints,
		})
	}
	return out, nil
}

func toutSafe(in []Stream) []Stream { return in }
