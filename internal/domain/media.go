package domain

import "time"

type Media struct {
	DurationMS int64  `json:"duration_ms"`
	Video      bool   `json:"video"`
	Audio      bool   `json:"audio"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	VideoCodec string `json:"video_codec"`
	AudioCodec string `json:"audio_codec"`
}
type Source struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Digest    string    `json:"digest"`
	Bytes     int64     `json:"bytes"`
	Media     Media     `json:"media"`
	CreatedAt time.Time `json:"created_at"`
}
type Span struct {
	SourceID string  `json:"source_id"`
	InMS     int64   `json:"in_ms"`
	OutMS    int64   `json:"out_ms"`
	GainDB   float64 `json:"gain_db"`
}
type Cue struct {
	SourceID string `json:"source_id"`
	StartMS  int64  `json:"start_ms"`
	EndMS    int64  `json:"end_ms"`
	Text     string `json:"text"`
}

// Marker is a source-anchored key visual. It belongs to the caption/editorial
// side of the workflow and is expressed in source time, the same coordinate
// system as cues and spans.
type Marker struct {
	SourceID string `json:"source_id"`
	AtMS     int64  `json:"at_ms"`
	Label    string `json:"label"`
}

// Transition requests an overlap dissolve at the boundary Before span index
// BeforeIndex (1..len(spans)-1). RequestedMS is the editor's chosen overlap;
// the timeline solver may resolve a smaller value when available headroom,
// protected dialogue or key visuals require it, and records the reason.
type Transition struct {
	BeforeIndex int   `json:"before_index"`
	RequestedMS int64 `json:"requested_ms"`
}

// TransitionReason explains a single reviewable resolution decision.
type TransitionReason struct {
	BeforeIndex int    `json:"before_index"`
	Code        string `json:"code"`
	Detail      string `json:"detail"`
}

// TransitionPlan is the resolved, frozen relationship between two
// neighbouring placements. EffectiveMS is the overlap actually encoded.
type TransitionPlan struct {
	BeforeIndex int                `json:"before_index"`
	RequestedMS int64              `json:"requested_ms"`
	EffectiveMS int64              `json:"effective_ms"`
	Reasons     []TransitionReason `json:"reasons"`
}
type Sheet struct {
	ID          string       `json:"id"`
	Revision    int          `json:"revision"`
	Name        string       `json:"name"`
	Spans       []Span       `json:"spans"`
	Cues        []Cue        `json:"cues"`
	Markers     []Marker     `json:"markers,omitempty"`
	Transitions []Transition `json:"transitions,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}
type Recipe struct {
	ID         string  `json:"id"`
	Revision   int     `json:"revision"`
	Name       string  `json:"name"`
	Format     string  `json:"format"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	FPS        int     `json:"fps"`
	SampleRate int     `json:"sample_rate"`
	Normalize  bool    `json:"normalize"`
	TargetLUFS float64 `json:"target_lufs"`
}
type Placement struct {
	SourceID    string  `json:"source_id"`
	SourceInMS  int64   `json:"source_in_ms"`
	SourceOutMS int64   `json:"source_out_ms"`
	OutputInMS  int64   `json:"output_in_ms"`
	OutputOutMS int64   `json:"output_out_ms"`
	GainDB      float64 `json:"gain_db"`
}
type Plan struct {
	DurationMS  int64            `json:"duration_ms"`
	Placements  []Placement      `json:"placements"`
	Transitions []TransitionPlan `json:"transitions,omitempty"`
	Recipe      Recipe           `json:"recipe"`
	VTT         string           `json:"vtt"`
}
