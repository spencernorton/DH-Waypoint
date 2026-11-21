package leanmode

import (
	"bytes"
	"context"
	"encoding/hex"
	"slices"
	"sort"
	"time"
)

// DiffOptions bounds a non-mutating comparison between two LEAN MODE YAML profiles.
type DiffOptions struct {
	TitleBatchSize int
	MaxTitles      int // 0 = evaluate every matching title
	MaxSamples     int // default 25
	Now            time.Time
}

// DiffSample records one concrete slot winner change between two profiles.
type DiffSample struct {
	SlotKey    string `json:"slot_key"`
	ChangeType string `json:"change_type"` // changed | newly_filled | newly_empty
	FromHash   string `json:"from_hash,omitempty"`
	FromName   string `json:"from_name,omitempty"`
	ToHash     string `json:"to_hash,omitempty"`
	ToName     string `json:"to_name,omitempty"`
}

// DiffReport summarizes slot-by-slot differences between two profiles without
// modifying any database rows.
type DiffReport struct {
	FromID           string       `json:"from_id"`
	FromDigest       string       `json:"from_digest"`
	ToID             string       `json:"to_id"`
	ToDigest         string       `json:"to_digest"`
	TitlesCompared   int          `json:"titles_compared"`
	SlotsCompared    int          `json:"slots_compared"`
	Unchanged        int          `json:"unchanged_winners"`
	Changed          int          `json:"changed_winners"`
	NewlyFilled      int          `json:"newly_filled_slots"`
	NewlyEmpty       int          `json:"newly_empty_slots"`
	FromWinnersRank1 int          `json:"from_winners_rank_1"`
	FromKeptTotal    int          `json:"from_kept_total"`
	ToWinnersRank1   int          `json:"to_winners_rank_1"`
	ToKeptTotal      int          `json:"to_kept_total"`
	SampleChanges    []DiffSample `json:"sample_changes"`
}

func effectivePubProcessor(cfg ProfileConfig) string {
	if cfg.Required.RequirePublicationReview != "" {
		return cfg.Required.RequirePublicationReview
	}
	if cfg.Required.RequireArmigerApproved {
		return "armiger-ultra"
	}
	return ""
}

// DiffTitleDecisions compares two deterministic TitleDecisions for the same
// title and appends slot stats and change samples into rep.
func DiffTitleDecisions(fromDec, toDec TitleDecision, rep *DiffReport, maxSamples int) {
	fromSlots := make(map[string]SlotDecision, len(fromDec.Slots))
	toSlots := make(map[string]SlotDecision, len(toDec.Slots))
	slotSet := make(map[string]struct{}, len(fromDec.Slots)+len(toDec.Slots))

	for _, sd := range fromDec.Slots {
		fromSlots[sd.SlotKey] = sd
		slotSet[sd.SlotKey] = struct{}{}
		rep.FromKeptTotal += len(sd.Winners)
		if len(sd.Winners) > 0 {
			rep.FromWinnersRank1++
		}
	}
	for _, sd := range toDec.Slots {
		toSlots[sd.SlotKey] = sd
		slotSet[sd.SlotKey] = struct{}{}
		rep.ToKeptTotal += len(sd.Winners)
		if len(sd.Winners) > 0 {
			rep.ToWinnersRank1++
		}
	}

	keys := make([]string, 0, len(slotSet))
	for k := range slotSet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		rep.SlotsCompared++
		var fromWin, toWin *Candidate
		if sd, ok := fromSlots[k]; ok && len(sd.Winners) > 0 {
			c := sd.Winners[0].Candidate
			fromWin = &c
		}
		if sd, ok := toSlots[k]; ok && len(sd.Winners) > 0 {
			c := sd.Winners[0].Candidate
			toWin = &c
		}

		switch {
		case fromWin == nil && toWin == nil:
			rep.Unchanged++
		case fromWin != nil && toWin != nil && bytes.Equal(fromWin.InfoHash, toWin.InfoHash):
			rep.Unchanged++
		case fromWin == nil && toWin != nil:
			rep.NewlyFilled++
			if len(rep.SampleChanges) < maxSamples {
				rep.SampleChanges = append(rep.SampleChanges, DiffSample{
					SlotKey:    k,
					ChangeType: "newly_filled",
					ToHash:     hex.EncodeToString(toWin.InfoHash),
					ToName:     toWin.Name,
				})
			}
		case fromWin != nil && toWin == nil:
			rep.NewlyEmpty++
			if len(rep.SampleChanges) < maxSamples {
				rep.SampleChanges = append(rep.SampleChanges, DiffSample{
					SlotKey:    k,
					ChangeType: "newly_empty",
					FromHash:   hex.EncodeToString(fromWin.InfoHash),
					FromName:   fromWin.Name,
				})
			}
		default:
			rep.Changed++
			if len(rep.SampleChanges) < maxSamples {
				rep.SampleChanges = append(rep.SampleChanges, DiffSample{
					SlotKey:    k,
					ChangeType: "changed",
					FromHash:   hex.EncodeToString(fromWin.InfoHash),
					FromName:   fromWin.Name,
					ToHash:     hex.EncodeToString(toWin.InfoHash),
					ToName:     toWin.Name,
				})
			}
		}
	}
}

// DiffProfiles evaluates two LEAN MODE YAML profiles side by side over stored
// titles without modifying any tables, winners, or tombstones.
func (s *Store) DiffProfiles(ctx context.Context, fromCfg, toCfg ProfileConfig, opts DiffOptions) (DiffReport, error) {
	if err := fromCfg.Validate(); err != nil {
		return DiffReport{}, err
	}
	if err := toCfg.Validate(); err != nil {
		return DiffReport{}, err
	}
	pool, err := s.pool.Get()
	if err != nil {
		return DiffReport{}, err
	}
	if opts.TitleBatchSize <= 0 {
		opts.TitleBatchSize = 200
	}
	if opts.MaxSamples <= 0 {
		opts.MaxSamples = 25
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	contentTypes := append([]string(nil), fromCfg.Scope.ContentTypes...)
	for _, ct := range toCfg.Scope.ContentTypes {
		if !slices.Contains(contentTypes, ct) {
			contentTypes = append(contentTypes, ct)
		}
	}
	sort.Strings(contentTypes)

	samePub := effectivePubProcessor(fromCfg) == effectivePubProcessor(toCfg)

	rep := DiffReport{
		FromID:        fromCfg.ID,
		FromDigest:    fromCfg.ConfigDigest(),
		ToID:          toCfg.ID,
		ToDigest:      toCfg.ConfigDigest(),
		SampleChanges: []DiffSample{},
	}

	var lastType, lastSource, lastID string
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		limit := opts.TitleBatchSize
		if opts.MaxTitles > 0 {
			remaining := opts.MaxTitles - rep.TitlesCompared
			if remaining <= 0 {
				break
			}
			if remaining < limit {
				limit = remaining
			}
		}
		titles, err := fetchNextTitles(ctx, pool, contentTypes, lastType, lastSource, lastID, limit)
		if err != nil {
			return rep, err
		}
		if len(titles) == 0 {
			break
		}
		for _, tk := range titles {
			lastType, lastSource, lastID = tk.ContentType, tk.ContentSource, tk.ContentID
			rep.TitlesCompared++

			var fromCands, toCands []Candidate
			if samePub {
				cands, err := loadTitleCandidates(ctx, pool, fromCfg, tk, now)
				if err != nil {
					return rep, err
				}
				fromCands, toCands = cands, cands
			} else {
				var err error
				fromCands, err = loadTitleCandidates(ctx, pool, fromCfg, tk, now)
				if err != nil {
					return rep, err
				}
				toCands, err = loadTitleCandidates(ctx, pool, toCfg, tk, now)
				if err != nil {
					return rep, err
				}
			}

			var fromDec, toDec TitleDecision
			if slices.Contains(fromCfg.Scope.ContentTypes, tk.ContentType) {
				fromDec = ResolveTitle(fromCfg, tk.ContentType, tk.ContentSource, tk.ContentID, fromCands, nil, now)
			}
			if slices.Contains(toCfg.Scope.ContentTypes, tk.ContentType) {
				toDec = ResolveTitle(toCfg, tk.ContentType, tk.ContentSource, tk.ContentID, toCands, nil, now)
			}
			DiffTitleDecisions(fromDec, toDec, &rep, opts.MaxSamples)
		}
		if len(titles) < limit {
			break
		}
	}
	return rep, nil
}
