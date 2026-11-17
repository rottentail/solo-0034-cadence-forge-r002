package timeline

import (
	"cadenceforge/internal/domain"
	"fmt"
)

// Build turns a sheet revision into an exact source-to-output relationship.
//
// Overlap transitions make neighbouring placements share output time: the
// tail of span k-1 dissolves into the head of span k for EffectiveMS
// milliseconds. Two pieces of editorial content are protected from that
// shared window:
//
//   - cues (source-anchored dialogue intervals) must remain fully audible and
//     readable, so a dissolve may never cover a cue;
//   - markers (source-anchored key visuals) must remain visible, so a
//     dissolve may never cover a marker point.
//
// A short middle span competes for its own head and tail room with the two
// transitions around it. The solver never silently shrinks a requested
// overlap and never rejects a legal edit: it resolves the largest feasible
// overlap, left boundary first, and attaches a reviewable reason to every
// decision. The resolved relationship is what gets frozen into a render.
func Build(sheet domain.Sheet, recipe domain.Recipe, sources map[string]domain.Source) (domain.Plan, error) {
	if err := domain.ValidateSheet(sheet, sources); err != nil {
		return domain.Plan{}, err
	}
	if err := domain.ValidateRecipe(recipe); err != nil {
		return domain.Plan{}, err
	}
	plan := domain.Plan{Recipe: recipe, Placements: []domain.Placement{}, Transitions: []domain.TransitionPlan{}}

	requested := map[int]int64{}
	for _, t := range sheet.Transitions {
		requested[t.BeforeIndex] = t.RequestedMS
	}

	// effective[k] is the resolved overlap at the boundary before span k.
	effective := map[int]int64{}
	// tailBudget[k] tracks how much of span k's tail can still fund the
	// transition leaving it, after the transition entering it claimed head
	// room. This is the short-middle-span contention budget.
	tailBudget := map[int]int64{}
	for i, span := range sheet.Spans {
		tailBudget[i] = span.OutMS - span.InMS
	}

	for k := 1; k < len(sheet.Spans); k++ {
		request, declared := requested[k]
		if !declared {
			// Boundaries without a declared transition have no plan record,
			// keeping legacy sheets and the zero-overlap case distinguishable.
			continue
		}
		tp := domain.TransitionPlan{BeforeIndex: k, RequestedMS: request, Reasons: []domain.TransitionReason{}}
		addReason := func(code, detail string) {
			tp.Reasons = append(tp.Reasons, domain.TransitionReason{BeforeIndex: k, Code: code, Detail: detail})
		}

		// WAV has no picture to dissolve. Audio-only outputs keep a contiguous
		// concatenation; the requested relationship is retained on record as a
		// no-op rather than being invented in the mix.
		if recipe.Format == "wav" {
			if request > 0 {
				addReason("format_unsupported", "WAV outputs keep contiguous audio; picture overlap is not encoded")
			}
			plan.Transitions = append(plan.Transitions, tp)
			continue
		}
		outSpan := sheet.Spans[k-1]
		inSpan := sheet.Spans[k]
		if !sources[outSpan.SourceID].Media.Video || !sources[inSpan.SourceID].Media.Video {
			addReason("format_unsupported", "overlap dissolve requires video on both spans")
			plan.Transitions = append(plan.Transitions, tp)
			continue
		}

		durOut := outSpan.OutMS - outSpan.InMS
		durIn := inSpan.OutMS - inSpan.InMS

		// Structural caps. A dissolve needs at least one frame from each span,
		// and the outgoing span cannot spend tail room already consumed by the
		// transition entering it (the contention case).
		structural := min64(request, durOut-1, durIn-1, tailBudget[k-1])
		if structural < request {
			if request > tailBudget[k-1] && tailBudget[k-1] <= min64(durOut-1, durIn-1) {
				addReason("contention_clamped", fmt.Sprintf("span %d cannot fully fund both its entering and leaving overlap; the leaving overlap was reduced", k-1))
			} else {
				addReason("span_too_short", "overlap cannot exceed either adjacent span minus one frame")
			}
		}

		// Protection caps: dialogue and key visuals may not enter the shared
		// window. Binary-search the largest overlap that clears every
		// protected interval/point on each side of the boundary.
		capMS := structural
		for _, side := range []struct {
			span   domain.Span
			isTail bool
		}{
			{outSpan, true},
			{inSpan, false},
		} {
			lo, hi, clear := int64(0), capMS, int64(0)
			for lo <= hi {
				mid := (lo + hi) / 2
				if side.isTail {
					if tailClear(sheet, side.span, mid) {
						clear, lo = mid, mid+1
					} else {
						hi = mid - 1
					}
				} else {
					if headClear(sheet, side.span, mid) {
						clear, lo = mid, mid+1
					} else {
						hi = mid - 1
					}
				}
			}
			if clear < capMS {
				label := "head"
				if side.isTail {
					label = "tail"
				}
				addReason("protected_content", fmt.Sprintf("%s-side %s must remain outside the dissolve", label, protectedDescription(sheet, side.span)))
				capMS = clear
			}
		}

		tp.EffectiveMS = capMS
		effective[k] = capMS
		tailBudget[k-1] -= capMS
		tailBudget[k] -= capMS
		plan.Transitions = append(plan.Transitions, tp)
	}

	for i, span := range sheet.Spans {
		source := sources[span.SourceID]
		if recipe.Format == "mp4" && !source.Media.Video {
			return plan, domain.Invalid("MP4 span requires video: %s", source.ID)
		}
		if recipe.Format == "wav" && !source.Media.Audio {
			return plan, domain.Invalid("WAV span requires audio: %s", source.ID)
		}
		duration := span.OutMS - span.InMS
		in := plan.DurationMS
		plan.Placements = append(plan.Placements, domain.Placement{
			SourceID:    source.ID,
			SourceInMS:  span.InMS,
			SourceOutMS: span.OutMS,
			OutputInMS:  in,
			OutputOutMS: in + duration,
			GainDB:      span.GainDB,
		})
		plan.DurationMS = in + duration - effective[i+1]
	}
	return plan, nil
}

func min64(values ...int64) int64 {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// tailClear reports whether an overlap of x leaves the tail window
// [span.Out-x, span.Out) free of every protected cue interval and marker
// point sourced from the same material as the span.
func tailClear(sheet domain.Sheet, span domain.Span, x int64) bool {
	windowStart := span.OutMS - x
	for _, c := range sheet.Cues {
		if c.SourceID != span.SourceID {
			continue
		}
		if max64(c.StartMS, windowStart) < min64(c.EndMS, span.OutMS) {
			return false
		}
	}
	for _, m := range sheet.Markers {
		if m.SourceID != span.SourceID {
			continue
		}
		if m.AtMS >= windowStart && m.AtMS < span.OutMS {
			return false
		}
	}
	return true
}

// headClear mirrors tailClear for the head window [span.In, span.In+x).
func headClear(sheet domain.Sheet, span domain.Span, x int64) bool {
	windowEnd := span.InMS + x
	for _, c := range sheet.Cues {
		if c.SourceID != span.SourceID {
			continue
		}
		if max64(c.StartMS, span.InMS) < min64(c.EndMS, windowEnd) {
			return false
		}
	}
	for _, m := range sheet.Markers {
		if m.SourceID != span.SourceID {
			continue
		}
		if m.AtMS >= span.InMS && m.AtMS < windowEnd {
			return false
		}
	}
	return true
}

func protectedDescription(sheet domain.Sheet, span domain.Span) string {
	hasCue, hasMarker := false, false
	for _, c := range sheet.Cues {
		if c.SourceID == span.SourceID {
			hasCue = true
			break
		}
	}
	for _, m := range sheet.Markers {
		if m.SourceID == span.SourceID {
			hasMarker = true
			break
		}
	}
	switch {
	case hasCue && hasMarker:
		return "dialogue and key visual"
	case hasCue:
		return "dialogue"
	case hasMarker:
		return "key visual"
	default:
		return "content"
	}
}
