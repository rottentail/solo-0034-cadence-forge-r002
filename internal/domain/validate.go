package domain

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

func ValidateRecipe(r Recipe) error {
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 160 {
		return Invalid("recipe name must contain 1..160 bytes")
	}
	if r.Format != "mp4" && r.Format != "wav" {
		return Invalid("format must be mp4 or wav")
	}
	if r.SampleRate != 48000 && r.SampleRate != 44100 {
		return Invalid("sample_rate must be 44100 or 48000")
	}
	if r.Format == "mp4" {
		if r.Width < 64 || r.Height < 64 || r.Width > 1920 || r.Height > 1080 || r.Width%2 != 0 || r.Height%2 != 0 {
			return Invalid("invalid even video dimensions")
		}
		if r.FPS < 1 || r.FPS > 60 {
			return Invalid("fps must be 1..60")
		}
	}
	if r.Normalize && (math.IsNaN(r.TargetLUFS) || math.IsInf(r.TargetLUFS, 0) || r.TargetLUFS < -36 || r.TargetLUFS > -9) {
		return Invalid("normalization target must be -36..-9 LUFS")
	}
	return nil
}
func ValidateSheet(s Sheet, sources map[string]Source) error {
	if strings.TrimSpace(s.Name) == "" || len(s.Name) > 160 {
		return Invalid("sheet name must contain 1..160 bytes")
	}
	if len(s.Spans) == 0 || len(s.Spans) > 32 {
		return Invalid("a sheet requires 1..32 spans")
	}
	var duration int64
	for i, p := range s.Spans {
		source, ok := sources[p.SourceID]
		if !ok {
			return Missing("source", p.SourceID)
		}
		if p.InMS < 0 || p.OutMS <= p.InMS || p.OutMS > source.Media.DurationMS {
			return Invalid("span %d is outside source duration", i)
		}
		if p.OutMS-p.InMS < 100 {
			return Invalid("span %d is shorter than 100ms", i)
		}
		if math.IsNaN(p.GainDB) || math.IsInf(p.GainDB, 0) || p.GainDB < -60 || p.GainDB > 12 {
			return Invalid("span %d has invalid gain", i)
		}
		duration += p.OutMS - p.InMS
	}
	if duration > 120000 {
		return Invalid("sheet exceeds 120 seconds")
	}
	if len(s.Cues) > 500 {
		return Invalid("sheet exceeds 500 cues")
	}
	for i, c := range s.Cues {
		source, ok := sources[c.SourceID]
		if !ok {
			return Missing("cue source", c.SourceID)
		}
		if c.StartMS < 0 || c.EndMS <= c.StartMS || c.EndMS > source.Media.DurationMS {
			return Invalid("cue %d has invalid interval", i)
		}
		if !utf8.ValidString(c.Text) || strings.TrimSpace(c.Text) == "" || len(c.Text) > 2000 || strings.Contains(c.Text, "-->") || strings.ContainsAny(c.Text, "\x00\r") {
			return Invalid("cue %d has invalid text", i)
		}
	}
	if len(s.Markers) > 200 {
		return Invalid("sheet exceeds 200 markers")
	}
	for i, m := range s.Markers {
		source, ok := sources[m.SourceID]
		if !ok {
			return Missing("marker source", m.SourceID)
		}
		if m.AtMS < 0 || m.AtMS >= source.Media.DurationMS {
			return Invalid("marker %d is outside source duration", i)
		}
		if !utf8.ValidString(m.Label) || len(m.Label) > 200 || strings.ContainsAny(m.Label, "\x00\r") {
			return Invalid("marker %d has invalid label", i)
		}
	}
	// A transition dissolves the tail of span k-1 against the head of span k.
	// Boundary indices must be unique, in range, and the requested overlap is
	// bounded so a malformed sheet can never consume the whole composition.
	boundaries := map[int]bool{}
	for i, t := range s.Transitions {
		if t.BeforeIndex < 1 || t.BeforeIndex >= len(s.Spans) {
			return Invalid("transition %d does not sit between two spans", i)
		}
		if boundaries[t.BeforeIndex] {
			return Invalid("transition boundary %d is declared twice", t.BeforeIndex)
		}
		boundaries[t.BeforeIndex] = true
		if t.RequestedMS < 0 || t.RequestedMS > maxTransitionMS {
			return Invalid("transition %d overlap must be 0..%dms", i, maxTransitionMS)
		}
	}
	return nil
}

const maxTransitionMS = 10000

// validatePlan guards the frozen source-to-output relationship persisted on
// every render: placements stay in sheet order, overlaps stay non-negative
// and bounded, and the declared duration equals span time minus overlaps.
func validatePlan(p Plan, s Sheet) error {
	if len(p.Placements) != len(s.Spans) {
		return fmt.Errorf("plan placement count does not match sheet")
	}
	var expected int64
	for i, span := range s.Spans {
		pl := p.Placements[i]
		if pl.SourceID != span.SourceID || pl.SourceInMS != span.InMS || pl.SourceOutMS != span.OutMS {
			return fmt.Errorf("plan placement %d diverges from frozen sheet", i)
		}
		if i > 0 && pl.OutputInMS != p.Placements[i-1].OutputOutMS-overlapBefore(p, i) {
			return fmt.Errorf("plan placement %d ignores its transition overlap", i)
		}
		expected += span.OutMS - span.InMS
	}
	var overlaps int64
	for _, t := range p.Transitions {
		if t.BeforeIndex < 1 || t.BeforeIndex >= len(s.Spans) || t.EffectiveMS < 0 || t.EffectiveMS > t.RequestedMS || t.RequestedMS > maxTransitionMS {
			return fmt.Errorf("invalid frozen transition at boundary %d", t.BeforeIndex)
		}
		overlaps += t.EffectiveMS
	}
	expected -= overlaps
	if expected != p.DurationMS {
		return fmt.Errorf("plan duration %d does not match spans minus overlaps %d", p.DurationMS, expected)
	}
	return nil
}

func overlapBefore(p Plan, index int) int64 {
	for _, t := range p.Transitions {
		if t.BeforeIndex == index {
			return t.EffectiveMS
		}
	}
	return 0
}
func ValidateState(s State) error {
	if s.Schema != 1 || s.Sources == nil || s.Sheets == nil || s.Recipes == nil || s.Renders == nil {
		return fmt.Errorf("unsupported or incomplete state schema")
	}
	for id, source := range s.Sources {
		if source.ID != id || len(source.Digest) != 64 || source.Bytes <= 0 {
			return fmt.Errorf("invalid source %s", id)
		}
	}
	for id, versions := range s.Sheets {
		if len(versions) == 0 {
			return fmt.Errorf("empty sheet history")
		}
		for i, v := range versions {
			if v.ID != id || v.Revision != i+1 {
				return fmt.Errorf("invalid sheet history")
			}
			if err := ValidateSheet(v, s.Sources); err != nil {
				return err
			}
		}
	}
	for id, versions := range s.Recipes {
		if len(versions) == 0 {
			return fmt.Errorf("empty recipe history")
		}
		for i, v := range versions {
			if v.ID != id || v.Revision != i+1 {
				return fmt.Errorf("invalid recipe history")
			}
			if err := ValidateRecipe(v); err != nil {
				return err
			}
		}
	}
	keys := map[string]bool{}
	for id, r := range s.Renders {
		if id != r.ID || keys[r.Key] {
			return fmt.Errorf("invalid render identity")
		}
		keys[r.Key] = true
		switch r.State {
		case "queued", "running", "succeeded", "failed", "cancelled":
		default:
			return fmt.Errorf("invalid render state")
		}
		if (r.State == "succeeded") != (r.Artifact != nil) {
			return fmt.Errorf("artifact state mismatch")
		}
		if r.Sources == nil {
			return fmt.Errorf("missing render source snapshot")
		}
		if err := ValidateSheet(r.Sheet, r.Sources); err != nil {
			return err
		}
		if err := ValidateRecipe(r.Recipe); err != nil {
			return err
		}
		if err := validatePlan(r.Plan, r.Sheet); err != nil {
			return err
		}
	}
	var previous uint64
	for _, e := range s.Events {
		if e.Sequence <= previous || e.Sequence > s.Sequence {
			return fmt.Errorf("event sequence is invalid")
		}
		previous = e.Sequence
	}
	return nil
}
