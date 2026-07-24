package misc

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestLogSavingCredentialsRedactsIdentityAndPath(t *testing.T) {
	logger := log.StandardLogger()
	previousOutput := logger.Out
	previousLevel := logger.Level
	previousFormatter := logger.Formatter
	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetLevel(log.DebugLevel)
	logger.SetFormatter(&log.TextFormatter{DisableTimestamp: true})
	t.Cleanup(func() {
		logger.SetOutput(previousOutput)
		logger.SetLevel(previousLevel)
		logger.SetFormatter(previousFormatter)
	})

	LogSavingCredentials("/tmp/auths/codex-person@example.com-plus.json")

	got := output.String()
	if !strings.Contains(got, "Saving credentials") {
		t.Fatalf("log did not record credential persistence: %q", got)
	}
	for _, forbidden := range []string{"/tmp/auths", "person@example.com", "codex-person"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("credential persistence log leaked %q: %q", forbidden, got)
		}
	}
}
