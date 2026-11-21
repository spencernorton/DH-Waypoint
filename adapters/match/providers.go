package match

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxUpstreamResponseBytes = 2 << 20

var errUpstreamTransient = errors.New("upstream_transient")

// Query is the normalized title lookup sent to a metadata backend.
type Query struct {
	Title       string
	Year        int
	ContentType string
	IsAnime     bool
}

// ExternalCandidate is one ranked hit returned by an external metadata backend.
type ExternalCandidate struct {
	Source   string
	ID       string
	Title    string
	Year     int
	Aliases  []string
	Evidence []string
}

// Provider queries an external catalogue API for title matches.
type Provider interface {
	Name() string
	DefaultEndpoint() string
	ContentTypes() []string
	Search(ctx context.Context, client *http.Client, endpoint string, q Query) ([]ExternalCandidate, error)
}

func newProvider(cfg Config) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "anilist":
		return &aniListProvider{}, nil
	case "musicbrainz":
		ua := strings.TrimSpace(cfg.UserAgent)
		if ua == "" {
			ua = "DH-Waypoint-MatchAdapter/1.0 (https://github.com/spencernorton/DH-Waypoint)"
		}
		return &musicBrainzProvider{userAgent: ua}, nil
	case "igdb":
		if strings.TrimSpace(cfg.APIClientID) == "" || strings.TrimSpace(cfg.APIBearerToken) == "" {
			return nil, errors.New("igdb_requires_client_id_and_bearer_token")
		}
		return &igdbProvider{clientID: cfg.APIClientID, bearerToken: cfg.APIBearerToken}, nil
	case "openlibrary":
		ua := strings.TrimSpace(cfg.UserAgent)
		if ua == "" {
			ua = "DH-Waypoint-MatchAdapter/1.0 (https://github.com/spencernorton/DH-Waypoint)"
		}
		return &openLibraryProvider{userAgent: ua}, nil
	default:
		return nil, fmt.Errorf("unsupported_match_provider: %q", cfg.Provider)
	}
}

func readUpstreamBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("%w: status=%d", errUpstreamTransient, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream_status_%d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstreamTransient, err)
	}
	if len(body) > maxUpstreamResponseBytes {
		return nil, errors.New("upstream_response_too_large")
	}
	return body, nil
}

// --- AniList (Anime) ---

type aniListProvider struct{}

func (*aniListProvider) Name() string            { return "anilist" }
func (*aniListProvider) DefaultEndpoint() string { return "https://graphql.anilist.co" }
func (*aniListProvider) ContentTypes() []string  { return []string{"tv_show", "movie"} }

const aniListSearchQuery = `query ($search: String) {
  Page(page: 1, perPage: 5) {
    media(search: $search, type: ANIME, sort: SEARCH_MATCH) {
      id
      idMal
      seasonYear
      startDate { year }
      title { romaji english native }
      synonyms
    }
  }
}`

func (p *aniListProvider) Search(ctx context.Context, client *http.Client, endpoint string, q Query) ([]ExternalCandidate, error) {
	payload, err := json.Marshal(map[string]any{
		"query":     aniListSearchQuery,
		"variables": map[string]any{"search": q.Title},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstreamTransient, err)
	}
	body, err := readUpstreamBody(resp)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data struct {
			Page struct {
				Media []struct {
					ID         int `json:"id"`
					IDMal      int `json:"idMal"`
					SeasonYear int `json:"seasonYear"`
					StartDate  struct {
						Year int `json:"year"`
					} `json:"startDate"`
					Title struct {
						Romaji  string `json:"romaji"`
						English string `json:"english"`
						Native  string `json:"native"`
					} `json:"title"`
					Synonyms []string `json:"synonyms"`
				} `json:"media"`
			} `json:"Page"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]ExternalCandidate, 0, len(parsed.Data.Page.Media))
	for _, m := range parsed.Data.Page.Media {
		if m.ID <= 0 {
			continue
		}
		primary := strings.TrimSpace(m.Title.English)
		if primary == "" {
			primary = strings.TrimSpace(m.Title.Romaji)
		}
		if primary == "" {
			primary = strings.TrimSpace(m.Title.Native)
		}
		if primary == "" {
			continue
		}
		year := m.SeasonYear
		if year == 0 {
			year = m.StartDate.Year
		}
		aliases := make([]string, 0, 3+len(m.Synonyms))
		for _, a := range []string{m.Title.Romaji, m.Title.English, m.Title.Native} {
			if a = strings.TrimSpace(a); a != "" && a != primary {
				aliases = append(aliases, a)
			}
		}
		aliases = append(aliases, m.Synonyms...)
		ev := []string{fmt.Sprintf("anilist:%d", m.ID)}
		if m.IDMal > 0 {
			ev = append(ev, fmt.Sprintf("mal:%d", m.IDMal))
		}
		out = append(out, ExternalCandidate{
			Source:   "anilist",
			ID:       strconv.Itoa(m.ID),
			Title:    primary,
			Year:     year,
			Aliases:  aliases,
			Evidence: ev,
		})
	}
	return out, nil
}

// --- MusicBrainz (Music) ---

type musicBrainzProvider struct {
	userAgent string
}

func (*musicBrainzProvider) Name() string            { return "musicbrainz" }
func (*musicBrainzProvider) DefaultEndpoint() string { return "https://musicbrainz.org/ws/2/release-group" }
func (*musicBrainzProvider) ContentTypes() []string  { return []string{"music"} }

func (p *musicBrainzProvider) Search(ctx context.Context, client *http.Client, endpoint string, q Query) ([]ExternalCandidate, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	params := u.Query()
	params.Set("query", q.Title)
	params.Set("fmt", "json")
	params.Set("limit", "5")
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstreamTransient, err)
	}
	body, err := readUpstreamBody(resp)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		ReleaseGroups []struct {
			ID               string `json:"id"`
			Title            string `json:"title"`
			FirstReleaseDate string `json:"first-release-date"`
			ArtistCredit     []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
		} `json:"release-groups"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]ExternalCandidate, 0, len(parsed.ReleaseGroups))
	for _, rg := range parsed.ReleaseGroups {
		if strings.TrimSpace(rg.ID) == "" || strings.TrimSpace(rg.Title) == "" {
			continue
		}
		var aliases []string
		if len(rg.ArtistCredit) > 0 && strings.TrimSpace(rg.ArtistCredit[0].Name) != "" {
			artist := strings.TrimSpace(rg.ArtistCredit[0].Name)
			aliases = append(aliases, artist+" - "+rg.Title, artist+" "+rg.Title)
		}
		out = append(out, ExternalCandidate{
			Source:   "musicbrainz",
			ID:       strings.TrimSpace(rg.ID),
			Title:    strings.TrimSpace(rg.Title),
			Year:     parseLeadingYear(rg.FirstReleaseDate),
			Aliases:  aliases,
			Evidence: []string{"musicbrainz:" + strings.TrimSpace(rg.ID)},
		})
	}
	return out, nil
}

// --- IGDB (Games) ---

type igdbProvider struct {
	clientID    string
	bearerToken string
}

func (*igdbProvider) Name() string            { return "igdb" }
func (*igdbProvider) DefaultEndpoint() string { return "https://api.igdb.com/v4/games" }
func (*igdbProvider) ContentTypes() []string  { return []string{"game"} }

func sanitizeApicalypseString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, ";", " ")
	return strings.TrimSpace(s)
}

func (p *igdbProvider) Search(ctx context.Context, client *http.Client, endpoint string, q Query) ([]ExternalCandidate, error) {
	apicalypse := fmt.Sprintf(
		`search "%s"; fields id,name,first_release_date,alternative_names.name; limit 5;`,
		sanitizeApicalypseString(q.Title),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(apicalypse))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Client-ID", p.clientID)
	req.Header.Set("Authorization", "Bearer "+p.bearerToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "text/plain")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstreamTransient, err)
	}
	body, err := readUpstreamBody(resp)
	if err != nil {
		return nil, err
	}
	var games []struct {
		ID               int64  `json:"id"`
		Name             string `json:"name"`
		FirstReleaseDate int64  `json:"first_release_date"`
		AlternativeNames []struct {
			Name string `json:"name"`
		} `json:"alternative_names"`
	}
	if err := json.Unmarshal(body, &games); err != nil {
		return nil, err
	}
	out := make([]ExternalCandidate, 0, len(games))
	for _, g := range games {
		if g.ID <= 0 || strings.TrimSpace(g.Name) == "" {
			continue
		}
		var year int
		if g.FirstReleaseDate > 0 {
			year = time.Unix(g.FirstReleaseDate, 0).UTC().Year()
		}
		aliases := make([]string, 0, len(g.AlternativeNames))
		for _, a := range g.AlternativeNames {
			if n := strings.TrimSpace(a.Name); n != "" {
				aliases = append(aliases, n)
			}
		}
		out = append(out, ExternalCandidate{
			Source:   "igdb",
			ID:       strconv.FormatInt(g.ID, 10),
			Title:    strings.TrimSpace(g.Name),
			Year:     year,
			Aliases:  aliases,
			Evidence: []string{fmt.Sprintf("igdb:%d", g.ID)},
		})
	}
	return out, nil
}

// --- OpenLibrary (Books & Audiobooks) ---

type openLibraryProvider struct {
	userAgent string
}

func (*openLibraryProvider) Name() string            { return "openlibrary" }
func (*openLibraryProvider) DefaultEndpoint() string { return "https://openlibrary.org/search.json" }
func (*openLibraryProvider) ContentTypes() []string  { return []string{"book", "audiobook"} }

func (p *openLibraryProvider) Search(ctx context.Context, client *http.Client, endpoint string, q Query) ([]ExternalCandidate, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	params := u.Query()
	params.Set("q", q.Title)
	params.Set("limit", "5")
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstreamTransient, err)
	}
	body, err := readUpstreamBody(resp)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Docs []struct {
			Key              string   `json:"key"`
			Title            string   `json:"title"`
			FirstPublishYear int      `json:"first_publish_year"`
			AuthorName       []string `json:"author_name"`
		} `json:"docs"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]ExternalCandidate, 0, len(parsed.Docs))
	for _, d := range parsed.Docs {
		id := strings.TrimPrefix(strings.TrimSpace(d.Key), "/works/")
		if id == "" || strings.TrimSpace(d.Title) == "" {
			continue
		}
		var aliases []string
		if len(d.AuthorName) > 0 && strings.TrimSpace(d.AuthorName[0]) != "" {
			author := strings.TrimSpace(d.AuthorName[0])
			aliases = append(aliases, author+" - "+d.Title, d.Title+" "+author)
		}
		out = append(out, ExternalCandidate{
			Source:   "openlibrary",
			ID:       id,
			Title:    strings.TrimSpace(d.Title),
			Year:     d.FirstPublishYear,
			Aliases:  aliases,
			Evidence: []string{"openlibrary:" + id},
		})
	}
	return out, nil
}

func parseLeadingYear(raw string) int {
	if len(raw) < 4 {
		return 0
	}
	y, err := strconv.Atoi(raw[:4])
	if err != nil || y < 1800 || y > 2200 {
		return 0
	}
	return y
}
