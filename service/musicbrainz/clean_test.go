package musicbrainz

import "testing"

func TestDropForeignChars(t *testing.T) {
	cleaner := NewMetadataCleaner("Latin")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "latin is untouched", in: "Fleetwood Mac", want: "Fleetwood Mac"},
		{
			// Stripping leaves only a prolonged sound mark, which is script-neutral
			// and would otherwise become the whole search query.
			name: "japanese title is kept whole",
			in:   "シェリー",
			want: "シェリー",
		},
		{name: "cyrillic title is kept whole", in: "Плот", want: "Плот"},
		{name: "japanese artist is kept whole", in: "尾崎豊", want: "尾崎豊"},
		{
			name: "mostly latin drops the rest",
			in:   "Sakura ハナ",
			want: "Sakura",
		},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleaner.DropForeignChars(tt.in); got != tt.want {
				t.Errorf("DropForeignChars(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanRecording(t *testing.T) {
	cleaner := NewMetadataCleaner("Latin")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain title", in: "Dreams", want: "Dreams"},
		{name: "remaster suffix", in: "Dreams - 2004 Remaster", want: "Dreams"},
		{name: "feat suffix", in: "Stay (feat. Bryan Adams)", want: "Stay"},
		{name: "radio edit", in: "One Kiss (Radio Edit)", want: "One Kiss"},
		{name: "meaningful parenthetical survives", in: "Everlong (Acoustic)", want: "Everlong"},
		{name: "unbalanced brackets are left alone", in: "Dreams (outtake", want: "Dreams (outtake"},
		{name: "non-latin title survives", in: "シェリー", want: "シェリー"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := cleaner.CleanRecording(tt.in); got != tt.want {
				t.Errorf("CleanRecording(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanArtist(t *testing.T) {
	cleaner := NewMetadataCleaner("Latin")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "single artist", in: "Fleetwood Mac", want: "Fleetwood Mac"},
		{name: "truncates at comma", in: "Calvin Harris, Dua Lipa", want: "Calvin Harris"},
		{name: "truncates at ampersand", in: "Calvin Harris & Dua Lipa", want: "Calvin Harris"},
		{name: "non-latin artist survives", in: "尾崎豊", want: "尾崎豊"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := cleaner.CleanArtist(tt.in); got != tt.want {
				t.Errorf("CleanArtist(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
