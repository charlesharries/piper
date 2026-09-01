package models

import (
	"strings"

	"github.com/spf13/viper"
)

// defaultSubmissionAgent identifies this build of piper when nothing is
// configured. The lexicon wants `<app-identifier>/<version>`.
const defaultSubmissionAgent = "piper/v0.0.10"

// SubmissionAgent reports the agent string piper identifies itself with. It
// reads the loaded config, so callers must not resolve it before config.Load()
// has run -- which is why the MusicBrainz user agent is built per request.
func SubmissionAgent() string {
	if agent := strings.TrimSpace(viper.GetString("app.submission_agent")); agent != "" {
		return agent
	}
	return defaultSubmissionAgent
}
