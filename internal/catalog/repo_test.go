package catalog

import "testing"

func TestSingularNameFollowsTheIngestRule(t *testing.T) {
	cases := map[string]string{
		"eggs":             "egg",
		"tomatoes":         "tomato",
		"cherry tomatoes":  "cherry tomato",
		"berries":          "berry",
		"bay leaves":       "bay leaf",
		"brussels sprouts": "brussel sprouts",
		"egg  yolks":       "egg yolk",
		"cloves":           "clove",
		// Kept as they are, because the catalog holds them in this form.
		"asparagus": "asparagus",
		"oats":      "oats",
		"molasses":  "molasses",
		"peas":      "peas",
		// Odd, but these are the catalog's own spellings.
		"peaches": "peache",
		"mangoes": "mangoe",
		// Not plurals, and too short or doubled to be touched.
		"egg":   "egg",
		"gas":   "gas",
		"glass": "glass",
		"":      "",
	}
	for input, want := range cases {
		if got := singularName(input); got != want {
			t.Errorf("singularName(%q) = %q, want %q", input, got, want)
		}
	}
}
