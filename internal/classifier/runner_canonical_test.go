package classifier

import (
	"context"
	"errors"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/evidence"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

// fakeStore is a minimal CanonicalStore for testing. Returns the
// configured label and error; tracks the last info_hash requested.
type fakeStore struct {
	label *evidence.CanonicalLabel
	err   error
	last  []byte
}

func (s *fakeStore) CanonicalForInfoHash(_ context.Context, ih []byte) (*evidence.CanonicalLabel, error) {
	s.last = append([]byte(nil), ih...)
	return s.label, s.err
}

// fakeRunner records whether it was invoked. Tests assert this to
// verify preemption short-circuited or fell through correctly.
type fakeRunner struct {
	called  bool
	result  classification.Result
	err     error
	torrent model.Torrent
}

func (r *fakeRunner) Run(_ context.Context, _ string, _ Flags, torrent model.Torrent) (classification.Result, error) {
	r.called = true
	r.torrent = torrent
	return r.result, r.err
}

// torrentWithHash is a convenience for tests — returns a minimal model
// torrent whose info_hash is a well-known byte pattern so assertions
// on the store's seen value are readable.
func torrentWithHash(b byte) model.Torrent {
	var ih protocol.InfoHash
	for i := range ih {
		ih[i] = b
	}
	return model.Torrent{InfoHash: ih}
}

func TestCanonicalMovieConstrainsInner(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		ResolvedSource: evidence.SourceRadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x01))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("movie labels must continue through identity enrichment")
	}
	if inner.torrent.Hint.ContentType != model.ContentTypeMovie {
		t.Fatalf("expected authoritative movie hint, got %+v", inner.torrent.Hint)
	}
}

func TestCanonicalTvShowConstrainsInner(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeTV,
		ResolvedSource: evidence.SourceSonarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x02))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("TV labels must continue through identity enrichment")
	}
	if inner.torrent.Hint.ContentType != model.ContentTypeTvShow {
		t.Fatalf("expected authoritative TV hint, got %+v", inner.torrent.Hint)
	}
}

func TestCanonicalCatalogIDReplacesExistingHint(t *testing.T) {
	torrent := torrentWithHash(0x06)
	torrent.Hint = model.TorrentHint{
		ContentType:   model.ContentTypeMovie,
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("111"),
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		MediaID:        " TVDB : 222 ",
		ResolvedSource: evidence.SourceRadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if got := inner.torrent.Hint.ContentSource.String; got != model.SourceTvdb {
		t.Fatalf("expected canonical source tvdb, got %q", got)
	}
	if got := inner.torrent.Hint.ContentID.String; got != "222" {
		t.Fatalf("expected canonical ID 222, got %q", got)
	}
}

func TestCanonicalArrLocalIDPreservesCompatibleIdentity(t *testing.T) {
	torrent := torrentWithHash(0x07)
	torrent.Hint = model.TorrentHint{
		ContentType:   model.ContentTypeTvShow,
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("333"),
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeTV,
		MediaID:        "sonarr:42",
		ResolvedSource: evidence.SourceSonarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if got := inner.torrent.Hint.ContentSource.String; got != model.SourceTmdb {
		t.Fatalf("arr-local ID must not replace catalog identity, got source %q", got)
	}
	if got := inner.torrent.Hint.ContentID.String; got != "333" {
		t.Fatalf("arr-local ID must preserve compatible content ID, got %q", got)
	}
}

func TestCanonicalTypeConflictClearsPriorIdentity(t *testing.T) {
	torrent := torrentWithHash(0x08)
	torrent.Hint = model.TorrentHint{
		ContentType:   model.ContentTypeMovie,
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("444"),
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeTV,
		ResolvedSource: evidence.SourceSonarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if inner.torrent.Hint.ContentType != model.ContentTypeTvShow {
		t.Fatalf("expected canonical TV type, got %+v", inner.torrent.Hint)
	}
	if inner.torrent.Hint.ContentSource.Valid || inner.torrent.Hint.ContentID.Valid {
		t.Fatalf("cross-type prior identity must be cleared, got %+v", inner.torrent.Hint)
	}
}

func TestCanonicalMoviePreservesExistingAttachmentEndToEnd(t *testing.T) {
	content := model.Content{
		Type:   model.ContentTypeMovie,
		Source: model.SourceTmdb,
		ID:     "555",
		Title:  "Existing Match",
	}
	torrent := torrentWithHash(0x09)
	torrent.Contents = []model.TorrentContent{{
		ContentType:   model.NewNullContentType(model.ContentTypeMovie),
		ContentSource: model.NewNullString(model.SourceTmdb),
		ContentID:     model.NewNullString("555"),
		Content:       content,
	}}
	inner := runner{
		workflows: map[string]action{
			"test": {run: func(ctx executionContext) (classification.Result, error) {
				return ctx.result, nil
			}},
		},
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		MediaID:        "radarr:12",
		ResolvedSource: evidence.SourceRadarr,
	}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	res, err := r.Run(context.Background(), "test", Flags{}, torrent)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == nil || res.Content.ID != "555" {
		t.Fatalf("canonical rematch downgraded existing attachment: %+v", res.Content)
	}
}

func TestCanonicalAmbiguousExistingAttachmentsAreNotChosen(t *testing.T) {
	torrent := torrentWithHash(0x0B)
	torrent.Contents = []model.TorrentContent{
		{
			ContentType:   model.NewNullContentType(model.ContentTypeMovie),
			ContentSource: model.NewNullString(model.SourceTmdb),
			ContentID:     model.NewNullString("1"),
		},
		{
			ContentType:   model.NewNullContentType(model.ContentTypeMovie),
			ContentSource: model.NewNullString(model.SourceTmdb),
			ContentID:     model.NewNullString("2"),
		},
	}
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		MediaID:        "radarr:12",
		ResolvedSource: evidence.SourceRadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	if _, err := r.Run(context.Background(), "", Flags{}, torrent); err != nil {
		t.Fatal(err)
	}
	if inner.torrent.Hint.ContentSource.Valid || inner.torrent.Hint.ContentID.Valid {
		t.Fatalf("ambiguous existing identities must not be selected: %+v", inner.torrent.Hint)
	}
}

func TestCanonicalNonVideoStillPreempts(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeBook,
		ResolvedSource: evidence.SourceReadarr,
	}}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	res, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x0A))
	if err != nil {
		t.Fatal(err)
	}
	if inner.called {
		t.Fatal("non-video type-only labels should retain the inexpensive preempt path")
	}
	if res.ContentType.ContentType != model.ContentTypeEbook {
		t.Fatalf("expected ebook, got %+v", res.ContentType)
	}
}

func TestFallThroughOnNoLabel(t *testing.T) {
	store := &fakeStore{label: nil}
	inner := &fakeRunner{result: classification.Result{}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x03))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("inner must be called when no canonical label exists")
	}
}

func TestFallThroughOnLookupError(t *testing.T) {
	store := &fakeStore{err: errors.New("pool down")}
	inner := &fakeRunner{result: classification.Result{}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x04))
	if err != nil {
		t.Fatal("availability must dominate: a store error should not fail classification")
	}
	if !inner.called {
		t.Fatal("inner must be called when canonical lookup fails")
	}
}

func TestFallThroughOnUnknownMediaType(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeUnknown,
		ResolvedSource: evidence.SourceQBittorrent,
	}}
	inner := &fakeRunner{result: classification.Result{}}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, err := r.Run(context.Background(), "", Flags{}, torrentWithHash(0x05))
	if err != nil {
		t.Fatal(err)
	}
	if !inner.called {
		t.Fatal("inner must run when canonical media_type is unknown")
	}
}

func TestCanonicalRunnerNonVideoNotHinted(t *testing.T) {
	labels := &fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "South Park S01E01 1080p WEB", "tmdb:2190"),
		tvLabel(2, "South Park S02E01 1080p WEB", "tmdb:2190"),
	}}

	t.Run("withTitleEvidence exe not hinted", func(t *testing.T) {
		inner := &fakeRunner{}
		r := NewCanonicalRunner(inner, &fakeStore{}, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels)))
		exeTorrent := model.Torrent{
			InfoHash: hashOf(9), Name: "South Park S29E04 1080p WEB H264-FLUX.exe",
			FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("exe"), Size: 1000,
		}
		// withTitleEvidence is exercised via Run (no canonical label).
		_, err := r.Run(context.Background(), "", Flags{}, exeTorrent)
		if err != nil {
			t.Fatal(err)
		}
		if !inner.torrent.Hint.IsNil() {
			t.Fatalf("exe torrent must not be hinted, got %+v", inner.torrent.Hint)
		}
	})

	t.Run("withTitleEvidence 7z not hinted", func(t *testing.T) {
		labels2 := &fakeTitleLabels{labels: []evidence.TitleLabel{
			tvLabel(1, "MobLand S01E01 1080p WEB", "tmdb:1001"),
			tvLabel(2, "MobLand S01E02 1080p WEB", "tmdb:1001"),
		}}
		inner := &fakeRunner{}
		r := NewCanonicalRunner(inner, &fakeStore{}, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels2)))
		tor := model.Torrent{
			InfoHash: hashOf(10), Name: "MobLand S02E07 1080p WEB H264.7z",
			FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("7z"), Size: 1000,
		}
		_, err := r.Run(context.Background(), "", Flags{}, tor)
		if err != nil {
			t.Fatal(err)
		}
		if !inner.torrent.Hint.IsNil() {
			t.Fatalf("7z torrent must not be hinted, got %+v", inner.torrent.Hint)
		}
	})

	t.Run("canonical TV label exe not hinted", func(t *testing.T) {
		inner := &fakeRunner{}
		store := &fakeStore{label: &evidence.CanonicalLabel{MediaType: evidence.MediaTypeTV, MediaID: "tmdb:2190", ResolvedSource: evidence.SourceSonarr}}
		r := NewCanonicalRunner(inner, store, NewPreemptMetrics())
		tor := model.Torrent{
			InfoHash: hashOf(9), Name: "South Park S29E04 1080p WEB H264-FLUX.exe",
			FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("exe"), Size: 1000,
		}
		_, err := r.Run(context.Background(), "", Flags{}, tor)
		if err != nil {
			t.Fatal(err)
		}
		if !inner.called {
			t.Fatal("inner must be called")
		}
		if !inner.torrent.Hint.IsNil() {
			t.Fatalf("canonical TV hint must not apply to exe, got %+v", inner.torrent.Hint)
		}
	})

	t.Run("canonical TV label 7z not hinted", func(t *testing.T) {
		inner := &fakeRunner{}
		store := &fakeStore{label: &evidence.CanonicalLabel{MediaType: evidence.MediaTypeTV, MediaID: "tmdb:1001", ResolvedSource: evidence.SourceSonarr}}
		r := NewCanonicalRunner(inner, store, NewPreemptMetrics())
		tor := model.Torrent{
			InfoHash: hashOf(10), Name: "MobLand S02E07 1080p WEB H264.7z",
			FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("7z"), Size: 1000,
		}
		_, err := r.Run(context.Background(), "", Flags{}, tor)
		if err != nil {
			t.Fatal(err)
		}
		if !inner.torrent.Hint.IsNil() {
			t.Fatalf("canonical TV hint must not apply to 7z, got %+v", inner.torrent.Hint)
		}
	})

	t.Run("multi-file non-video not hinted but video is", func(t *testing.T) {
		inner := &fakeRunner{}
		r := NewCanonicalRunner(inner, &fakeStore{}, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels)))
		multi := model.Torrent{
			InfoHash: hashOf(12), Name: "South Park S29E04 1080p WEB", FilesStatus: model.FilesStatusMulti,
			Files: []model.TorrentFile{
				{Path: "show.exe", Size: 100, Extension: model.NewNullString("exe")},
				{Path: "show.rar", Size: 100, Extension: model.NewNullString("rar")},
				{Path: "show.nfo", Size: 10, Extension: model.NewNullString("nfo")},
			},
		}
		_, err := r.Run(context.Background(), "", Flags{}, multi)
		if err != nil {
			t.Fatal(err)
		}
		if !inner.torrent.Hint.IsNil() {
			t.Fatalf("multi non-video must not be hinted, got %+v", inner.torrent.Hint)
		}

		inner2 := &fakeRunner{}
		r2 := NewCanonicalRunner(inner2, &fakeStore{}, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels)))
		withVideo := model.Torrent{
			InfoHash: hashOf(13), Name: "South Park S29E04 1080p WEB", FilesStatus: model.FilesStatusMulti,
			Files: []model.TorrentFile{
				{Path: "show.mkv", Size: 1000, Extension: model.NewNullString("mkv")},
				{Path: "show.nfo", Size: 10, Extension: model.NewNullString("nfo")},
			},
		}
		_, err = r2.Run(context.Background(), "", Flags{}, withVideo)
		if err != nil {
			t.Fatal(err)
		}
		if inner2.torrent.Hint.ContentType != model.ContentTypeTvShow {
			t.Fatalf("multi with video must be hinted tv_show, got %+v", inner2.torrent.Hint)
		}
		if inner2.torrent.Hint.ContentID.String != "2190" {
			t.Fatalf("expected tmdb 2190, got %+v", inner2.torrent.Hint)
		}
	})
}

func TestCanonicalRunnerCoreClassifierExeIsSoftware(t *testing.T) {
	mocks := newTestClassifierMocks(t)
	source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
	if err != nil {
		t.Fatal(err)
	}
	inner, err := mocks.compiler.Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	labels := &fakeTitleLabels{labels: []evidence.TitleLabel{
		tvLabel(1, "South Park S01E01 1080p WEB", "tmdb:2190"),
		tvLabel(2, "South Park S02E01 1080p WEB", "tmdb:2190"),
	}}
	r := NewCanonicalRunner(inner, &fakeStore{}, NewPreemptMetrics(), WithTitleEvidence(NewTitleEvidence(labels)))
	tor := model.Torrent{
		InfoHash: hashOf(9), Name: "South Park S29E04 1080p WEB H264-FLUX.exe",
		FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("exe"), Size: 50_000_000,
	}
	res, err := r.Run(context.Background(), "default", Flags{}, tor)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ContentType.Valid || res.ContentType.ContentType != model.ContentTypeSoftware {
		t.Fatalf("expected software, got %+v", res.ContentType)
	}
	if res.Content != nil {
		t.Fatalf("must not attach TV content, got %+v", res.Content)
	}
}

func TestInfoHashBytesForwardedToStore(t *testing.T) {
	store := &fakeStore{label: nil}
	inner := &fakeRunner{}
	r := NewCanonicalRunner(inner, store, NewPreemptMetrics())

	_, _ = r.Run(context.Background(), "", Flags{}, torrentWithHash(0xAB))
	if len(store.last) != 20 {
		t.Fatalf("expected 20-byte infohash forwarded to store, got %d", len(store.last))
	}
	for _, b := range store.last {
		if b != 0xAB {
			t.Fatalf("unexpected byte in forwarded infohash: %x", b)
		}
	}
}
