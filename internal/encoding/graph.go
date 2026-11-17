package encoding

import (
	"cadenceforge/internal/domain"
	"cadenceforge/internal/media"
	"fmt"
	"strings"
)

func seconds(ms int64) string { return fmt.Sprintf("%.3f", float64(ms)/1000) }

// transitionAt returns the resolved transition whose boundary sits before
// span index spanIndex, or nil when the boundary has no effective overlap.
func transitionAt(plan domain.Plan, spanIndex int) *domain.TransitionPlan {
	for i := range plan.Transitions {
		t := &plan.Transitions[i]
		if t.BeforeIndex == spanIndex && t.EffectiveMS > 0 {
			return t
		}
	}
	return nil
}

func Arguments(render domain.Render, paths map[string]string, output string) ([]string, error) {
	r := render.Recipe
	args := []string{"-nostdin", "-y", "-hide_banner", "-loglevel", "error", "-filter_complex_threads", "1"}
	filters := []string{}
	for i, p := range render.Plan.Placements {
		path, ok := paths[p.SourceID]
		if !ok {
			return nil, fmt.Errorf("source object missing")
		}
		args = append(args, "-protocol_whitelist", "file,pipe", "-format_whitelist", media.Formats, "-i", path)
		durationMS := p.OutputOutMS - p.OutputInMS
		audio := fmt.Sprintf("[%d:a:0]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS,aresample=%d,aformat=sample_fmts=fltp:channel_layouts=stereo,volume=%.3fdB[a%d]", i, seconds(p.SourceInMS), seconds(p.SourceOutMS), r.SampleRate, p.GainDB, i)
		if !render.Sources[p.SourceID].Media.Audio {
			// Silent picture keeps its full video; its audio bed is synthesized
			// for exactly the placement length so dissolves stay continuous.
			audio = fmt.Sprintf("anullsrc=r=%d:cl=stereo,atrim=duration=%s,asetpts=PTS-STARTPTS[a%d]", r.SampleRate, seconds(durationMS), i)
		}
		filters = append(filters, audio)
		if r.Format == "mp4" {
			filters = append(filters, fmt.Sprintf("[%d:v:0]trim=start=%s:end=%s,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%d,format=yuv420p[v%d]", i, seconds(p.SourceInMS), seconds(p.SourceOutMS), r.Width, r.Height, r.Width, r.Height, r.FPS, i))
		}
	}
	if len(render.Plan.Placements) == 0 {
		return nil, fmt.Errorf("empty render")
	}

	if r.Format == "wav" || !hasDissolve(render.Plan) {
		// Contiguous path: plain concat. WAV never encodes picture overlap, and
		// MP4 without an effective overlap is identical to the baseline mix.
		joins := []string{}
		for i := range render.Plan.Placements {
			if r.Format == "mp4" {
				joins = append(joins, fmt.Sprintf("[v%d][a%d]", i, i))
			} else {
				joins = append(joins, fmt.Sprintf("[a%d]", i))
			}
		}
		if r.Format == "mp4" {
			filters = append(filters, strings.Join(joins, "")+fmt.Sprintf("concat=n=%d:v=1:a=1[vpre][acat]", len(joins)))
		} else {
			filters = append(filters, strings.Join(joins, "")+fmt.Sprintf("concat=n=%d:v=0:a=1[acat]", len(joins)))
		}
	} else {
		// Overlap path: fold placements with xfade (video) and acrossfade
		// (audio). Each dissolve shortens the running stream by exactly the
		// resolved overlap, so offsets are derived from the same frozen plan
		// the inspection step checks against.
		vLabel, aLabel := "v0", "a0"
		runningMS := render.Plan.Placements[0].OutputOutMS - render.Plan.Placements[0].OutputInMS
		for k := 1; k < len(render.Plan.Placements); k++ {
			spanMS := render.Plan.Placements[k].OutputOutMS - render.Plan.Placements[k].OutputInMS
			t := transitionAt(render.Plan, k)
			nextV, nextA := fmt.Sprintf("vx%d", k), fmt.Sprintf("ax%d", k)
			if t == nil {
				filters = append(filters,
					fmt.Sprintf("[%s][v%d]concat=n=2:v=1:a=0[%s]", vLabel, k, nextV),
					fmt.Sprintf("[%s][a%d]concat=n=2:v=0:a=1[%s]", aLabel, k, nextA))
				runningMS += spanMS
			} else {
				d := t.EffectiveMS
				offset := runningMS - d
				filters = append(filters,
					fmt.Sprintf("[%s][v%d]xfade=transition=fade:duration=%s:offset=%s[%s]", vLabel, k, seconds(d), seconds(offset), nextV),
					fmt.Sprintf("[%s][a%d]acrossfade=d=%s:c1=tri:c2=tri[%s]", aLabel, k, seconds(d), nextA))
				runningMS += spanMS - d
			}
			vLabel, aLabel = nextV, nextA
		}
		filters = append(filters, fmt.Sprintf("[%s]format=yuv420p[vpre]", vLabel))
		filters = append(filters, fmt.Sprintf("[%s]anull[acat]", aLabel))
	}

	if r.Normalize {
		// aformat after loudnorm keeps an explicit stereo layout on FFmpeg
		// builds whose loudnorm reinit drops the negotiated channel layout.
		filters = append(filters, fmt.Sprintf("[acat]loudnorm=I=%.2f:TP=-1.5:LRA=11,aresample=%d,aformat=sample_fmts=fltp:channel_layouts=stereo[aout]", r.TargetLUFS, r.SampleRate))
	} else {
		filters = append(filters, "[acat]anull[aout]")
	}
	args = append(args, "-filter_complex", strings.Join(filters, ";"))
	if r.Format == "mp4" {
		args = append(args, "-map", "[vpre]", "-map", "[aout]", "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-threads", "1", "-c:a", "aac", "-b:a", "160k", "-movflags", "+faststart")
	} else {
		args = append(args, "-map", "[aout]", "-c:a", "pcm_s16le")
	}
	return append(args, "-ar", fmt.Sprint(r.SampleRate), "-t", seconds(render.Plan.DurationMS), "-map_metadata", "-1", output), nil
}

func hasDissolve(plan domain.Plan) bool {
	for _, t := range plan.Transitions {
		if t.EffectiveMS > 0 {
			return true
		}
	}
	return false
}
