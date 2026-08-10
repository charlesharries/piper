# The Golden Set

This is an evaluation set for the MusicBrainz search. Each row is a set of metadata with inputs from e.g. Spotify or Apple Music or Last.fm, and an expected recording MusicBrainz ID and expected recording/release title.

Assertions should be made against expected recording/release title over expected MetaBrainz ID, because there are different IDs for, like, the same song on a bunch of different compilations, and what we're really looking for here is that the track details that we're saving to your PDS are right.

The track durations are important, here! A lot of the time they're the only thing that separates a studio recording from a live recording or an outtake or one of those recordings where there's like three songs in a single track or something.

## Lines without expectations

Some lines (like the classical music one) don't actually expect anything -- in this case we're just checking that the matcher finds _something_, since with classical music there's a lot of ambiguity around what's actually right for a given recording.