package main

import "slices"

// Ratings say which engine did best at each task, from the live test runs
// recorded in README.md (2026-10-17, one or two runs per
// engine and task). They are evidence, not opinion: an engine that has not
// run a task is untested, never guessed. Add a rating only with the run that
// earned it, and say what was seen.

// Rating tiers, best first.
const (
	tierBest     = "best"
	tierGood     = "good"
	tierUntested = ""
	tierWeak     = "weak"
)

// rating is how an engine did at one task, and what the run showed.
type rating struct {
	Tier string
	Note string
}

// tierRank orders tiers for sorting: best, good, untested, weak.
func tierRank(tier string) int {
	return map[string]int{tierBest: 0, tierGood: 1, tierUntested: 2, tierWeak: 3}[tier]
}

// tierMeter is the tier as a three-dot meter, so it reads without color.
func tierMeter(tier string) string {
	return map[string]string{tierBest: "●●●", tierGood: "●●○", tierWeak: "●○○"}[tier]
}

// imageRatings are keyed by purpose, with "+ref" for a request that starts
// from references.
var imageRatings = map[string]map[string]rating{
	"character": {
		"gpt-image-25-flare": {tierBest, "Four clean views that kept every detail asked for."},
		"qwen21":             {tierBest, "Four clean, even views."},
		"seedream5":          {tierBest, "Four consistent views."},
		"zimage":             {tierGood, "Consistent, but drew the side profile twice instead of a three-quarter view."},
	},
	"character+ref": {
		"gpt-image-25-sunburst": {tierBest, "Kept the face, outfit, and backpack in every view."},
		"qwen21-edit":           {tierBest, "Kept the face and outfit in every view."},
	},
	"environment": {
		"nanobanana21": {tierBest, "Four panels of one consistent place."},
		"seedream5":    {tierBest, "Four panels of one consistent place."},
		"krea3":        {tierBest, "Four panels of one consistent place."},
		"krea2":        {tierBest, "Four panels of one consistent place."},
		"qwen21-turbo": {tierGood, "A good sheet, but it painted signage in."},
		"zimage":       {tierWeak, "Kept the layout but lost the place across panels."},
	},
	"object": {
		"nanobanana21": {tierBest, "Clean, centered, and true to the description."},
	},
	"object+ref": {
		"nanobanana2": {tierBest, "Isolated the object faithfully."},
	},
	"place": {
		"krea3": {tierBest, "A clean, empty establishing shot."},
		"krea2": {tierBest, "A strong, moody establishing shot."},
	},
	"free+ref": {
		"seedream5": {tierWeak, "Combining two references changed the person's face and clothes."},
	},
}

// videoRatings are keyed by mode.
var videoRatings = map[string]map[string]rating{
	modeMotion: {
		"seedance25":       {tierBest, "Kept the reference's look and the clip's setting and shots."},
		"minimax-h3":       {tierBest, "Kept the reference's look and followed the clip's shots closely."},
		"wan-animate2":     {tierGood, "Follows the clip frame for frame; describe looks, not motion."},
		"wan3-r2v":         {tierGood, "Recast the performer and kept the setting, but not frame for frame."},
		"minimax-h3-local": {tierGood, "Copied the poses; the setting comes from your prompt, not the clip."},
	},
	modeStyle: {
		"wan3-style":  {tierGood, "Took the look in the opening; it fades as the shot widens."},
		"ltx23-style": {tierWeak, "The look fades after the first frames, and the output runs short."},
	},
}

// imageRating is an image engine's rating for a purpose, with references
// or without.
func imageRating(purpose string, refs bool, id string) rating {
	key := purpose
	if refs {
		key += "+ref"
	}
	return imageRatings[key][engineModelID(id)]
}

func videoRating(mode, id string) rating { return videoRatings[mode][engineModelID(id)] }

// purposeNoun names a purpose in a rating line.
func purposeNoun(purpose string, refs bool) string {
	n := map[string]string{"character": "character sheets", "environment": "environment sheets",
		"object": "objects", "place": "places", "free": "free prompts"}[purpose]
	if refs {
		n += " from references"
	}
	return n
}

// byRating orders engines best first, keeping the given order inside a tier.
func byRating[T any](items []T, tier func(T) string) {
	slices.SortStableFunc(items, func(a, b T) int { return tierRank(tier(a)) - tierRank(tier(b)) })
}
