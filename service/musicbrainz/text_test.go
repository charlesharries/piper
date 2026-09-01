package musicbrainz

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercases", in: "Blue Monday", want: "blue monday"},
		{name: "strips punctuation", in: "Power, Corruption & Lies", want: "power corruption and lies"},
		{name: "spells out ampersand", in: "Power Corruption and Lies", want: "power corruption and lies"},
		{name: "strips diacritics", in: "Beyoncé", want: "beyonce"},
		{name: "collapses whitespace", in: "  Dreams   ", want: "dreams"},
		{name: "keeps digits", in: "99 Problems", want: "99 problems"},
		{name: "punctuation becomes a break", in: "Mr.Brightside", want: "mr brightside"},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalize(tt.in); got != tt.want {
				t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The release "Power Corruption and Lies" and the release group
// "Power, Corruption & Lies" are the same album; normalization is what lets a
// title from Spotify match either.
func TestNormalizeMatchesReleaseAndGroupTitles(t *testing.T) {
	if normalize("Power, Corruption & Lies") != normalize("Power Corruption and Lies") {
		t.Errorf("expected release and release group titles to normalize alike")
	}
}

func TestSimilarity(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		wantMin float64
		wantMax float64
	}{
		{name: "identical", a: "dreams", b: "dreams", wantMin: 1, wantMax: 1},
		{name: "empty pair", a: "", b: "", wantMin: 0, wantMax: 0},
		{name: "one empty", a: "dreams", b: "", wantMin: 0, wantMax: 0},
		{name: "close", a: "rumours", b: "rumors", wantMin: 0.8, wantMax: 0.99},
		{name: "unrelated", a: "dreams", b: "bohemian rhapsody", wantMin: 0, wantMax: 0.3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := similarity(tt.a, tt.b)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("similarity(%q, %q) = %v, want within [%v, %v]", tt.a, tt.b, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestSplitQualifier(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantBase      string
		wantQualifier string
	}{
		{name: "parenthesised", in: "Dreams (outtake)", wantBase: "dreams", wantQualifier: "outtake"},
		{name: "bracketed", in: "Dreams [Live]", wantBase: "dreams", wantQualifier: "live"},
		{name: "dash suffix", in: "Dreams - 2004 Remaster", wantBase: "dreams", wantQualifier: "2004 remaster"},
		{name: "no qualifier", in: "Dreams", wantBase: "dreams", wantQualifier: ""},
		{name: "hyphenated title survives", in: "Jack-in-the-Box", wantBase: "jack in the box", wantQualifier: ""},
		{name: "edition suffix", in: "Rumours (Super Deluxe)", wantBase: "rumours", wantQualifier: "super deluxe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, qualifier := splitQualifier(tt.in)
			if base != tt.wantBase || qualifier != tt.wantQualifier {
				t.Errorf("splitQualifier(%q) = (%q, %q), want (%q, %q)",
					tt.in, base, qualifier, tt.wantBase, tt.wantQualifier)
			}
		})
	}
}

func TestIsVariantQualifier(t *testing.T) {
	tests := []struct {
		qualifier string
		want      bool
	}{
		{qualifier: "outtake", want: true},
		{qualifier: "live", want: true},
		{qualifier: "2004 remaster", want: true},
		{qualifier: "radio edit", want: true},
		{qualifier: "feat kanye west", want: false},
		{qualifier: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.qualifier, func(t *testing.T) {
			if got := isVariantQualifier(tt.qualifier); got != tt.want {
				t.Errorf("isVariantQualifier(%q) = %v, want %v", tt.qualifier, got, tt.want)
			}
		})
	}
}
