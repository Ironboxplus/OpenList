package scraper

import "testing"

func TestTitleSimilarityContainmentWeighsLength(t *testing.T) {
	below := []struct{ a, b string }{
		{"1917", "Kvinnlig fotboll i Gävle 1917"},      // a number inside a long title
		{"Leon", "Leoncavallo - I Giorni dello Sgombero"}, // inside a word
		{"Leon", "Leonardo"},
	}
	for _, c := range below {
		if s := titleSimilarity(c.a, c.b); s >= 0.6 {
			t.Errorf("%q vs %q = %.2f, want < 0.6", c.a, c.b, s)
		}
	}
	above := []struct {
		a, b string
		min  float32
	}{
		{"Léon", "Leon", 0.95},                   // diacritics fold
		{"Amélie", "Amelie", 0.95},
		{"1917", "1917", 1},
		{"The Godfather", "The Godfather Part II", 0.7},
		{"无间道", "无间道II", 0.7},
		{"Mo Dao Zu Shi", "mo dao zu shi", 0.95},
	}
	for _, c := range above {
		if s := titleSimilarity(c.a, c.b); s < c.min {
			t.Errorf("%q vs %q = %.2f, want >= %.2f", c.a, c.b, s, c.min)
		}
	}
	// a longer common title still beats a shorter one
	if titleSimilarity("The Godfather", "The Godfather Part II") <= titleSimilarity("The Godfather", "The Godfather Part II: The Complete Epic Saga Edition") {
		t.Error("the closer title must score higher")
	}
}
