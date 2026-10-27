package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestAppJS_NoEventEditing(t *testing.T) {
	// Read js/app.js from embedded Static(false)
	sys := Static(false)
	data, err := fs.ReadFile(sys, "js/app.js")
	if err != nil {
		t.Fatalf("failed to read js/app.js from Static(false): %v", err)
	}

	content := string(data)

	// Regex for api\(\s*'(POST|PUT|DELETE)'\s*,\s*`/households/\$\{[^}]*\}/events
	pattern := "api\\(\\s*'(POST|PUT|DELETE)'\\s*,\\s*`/households/\\$\\{[^}]*\\}/events"
	re := regexp.MustCompile(pattern)

	// Subtest to verify our regex catches old patterns
	t.Run("RegexDetectionSanityCheck", func(t *testing.T) {
		dummyOldCode := "await api('POST', `/households/${h.id}/events`, { title: 'Dinner' });"
		if !re.MatchString(dummyOldCode) {
			t.Fatalf("expected regex to match old event creation pattern, but did not")
		}
		dummyOldPut := "await api('PUT', `/households/${h.id}/events/${e.id}`, { title: 'Dinner' });"
		if !re.MatchString(dummyOldPut) {
			t.Fatalf("expected regex to match old event edit pattern, but did not")
		}
		dummyOldDel := "await api('DELETE', `/households/${h.id}/events/${e.id}`);"
		if !re.MatchString(dummyOldDel) {
			t.Fatalf("expected regex to match old event delete pattern, but did not")
		}
	})

	// Assertions on actual app.js content:
	// 1. Must not contain "Add event" string
	if strings.Contains(content, "Add event") {
		t.Errorf("expected js/app.js not to contain %q, but found it", "Add event")
	}

	// 2. Also check literal \"'Add event'\" is absent
	if strings.Contains(content, "'Add event'") {
		t.Errorf("expected js/app.js not to contain %q, but found it", "'Add event'")
	}

	// 3. Must not match regex for api\(\s*'(POST|PUT|DELETE)'\s*,\s*`/households/\$\{[^}]*\}/events
	if match := re.FindString(content); match != "" {
		t.Errorf("expected js/app.js not to match event mutation regex, but found match: %s", match)
	}

	// 4. Assert PUT to /calendar/entries/.../tags is present
	tagPutPattern := `api\(\s*'PUT'\s*,\s*` + "`" + `/households/\$\{h\.id\}/calendar/entries/\$\{[^}]+\}/tags` + "`"
	tagPutRe := regexp.MustCompile(tagPutPattern)
	if !tagPutRe.MatchString(content) {
		t.Errorf("expected js/app.js to call PUT /households/${h.id}/calendar/entries/${...}/tags, but pattern did not match")
	}

	// 5. Assert can_tag is referenced
	if !strings.Contains(content, "can_tag") {
		t.Errorf("expected js/app.js to reference can_tag, but it did not")
	}
}
