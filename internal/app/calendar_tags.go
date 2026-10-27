package app

import (
	"strings"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/db"
)

// Tagging a family calendar entry writes the chosen members' display names
// into the entry's Google title as one trailing group: "Dentist [Mom, Sam]".
// Google's title is the only place tags live; Chorus stores none.

// Reasons a member can't be tagged.
const (
	TagReasonDuplicateName = "duplicate_name"
	TagReasonReservedChar  = "reserved_character"
	TagReasonEmptyName     = "empty_name"
)

// TagMember is a household member as a tag choice. A member is taggable when
// their trimmed display name is non-empty, unique among the household's
// members (case-insensitively) and contains no ',', '[' or ']', so it reads
// back unambiguously from a title. Reason says why one is not.
type TagMember struct {
	UserID   uuid.UUID
	Name     string
	Taggable bool
	Reason   string
}

// tagMembers classifies the household's current members as tag choices, in
// the order given.
func tagMembers(rows []db.ListHouseholdMembersRow) []TagMember {
	counts := map[string]int{}
	for _, r := range rows {
		counts[tagKey(r.DisplayName)]++
	}
	out := make([]TagMember, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.DisplayName)
		m := TagMember{UserID: r.UserID, Name: name, Taggable: true}
		switch {
		case name == "":
			m.Taggable, m.Reason = false, TagReasonEmptyName
		case strings.ContainsAny(name, ",[]"):
			m.Taggable, m.Reason = false, TagReasonReservedChar
		case counts[tagKey(name)] > 1:
			m.Taggable, m.Reason = false, TagReasonDuplicateName
		}
		out = append(out, m)
	}
	return out
}

func tagKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// tagNames maps tag ids to the display names written into a title, in the
// order given and without repeats. Any id that is not a taggable member is
// returned in bad, and then names must not be used.
func tagNames(members []TagMember, ids []uuid.UUID) (names []string, bad []uuid.UUID) {
	byID := make(map[uuid.UUID]TagMember, len(members))
	for _, m := range members {
		byID[m.UserID] = m
	}
	seen := map[uuid.UUID]bool{}
	names = []string{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		m, ok := byID[id]
		if !ok || !m.Taggable {
			bad = append(bad, id)
			continue
		}
		names = append(names, m.Name)
	}
	return names, bad
}

// parseTagTitle splits title into its base and its tag group. The trailing
// " [..]" group (or a title that is only "[..]") is the tag group when at
// least one comma-separated name in it matches, trimmed and
// case-insensitively, a taggable member. tags are those members' ids in
// title order; kept are the group's other names, in order, which are written
// back after the chosen members. When no name matches, the whole title is
// the base and there are no tags.
func parseTagTitle(title string, members []TagMember) (base string, tags []uuid.UUID, kept []string) {
	tags, kept = []uuid.UUID{}, []string{}
	if !strings.HasSuffix(title, "]") {
		return title, tags, kept
	}
	open := strings.LastIndexByte(title, '[')
	if open < 0 || (open > 0 && title[open-1] != ' ') {
		return title, tags, kept
	}
	inner := title[open+1 : len(title)-1]
	if strings.ContainsAny(inner, "[]") {
		return title, tags, kept
	}
	byKey := map[string]uuid.UUID{}
	for _, m := range members {
		if m.Taggable {
			byKey[tagKey(m.Name)] = m.UserID
		}
	}
	seen := map[uuid.UUID]bool{}
	for _, part := range strings.Split(inner, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if id, ok := byKey[tagKey(name)]; ok {
			if !seen[id] {
				seen[id] = true
				tags = append(tags, id)
			}
			continue
		}
		kept = append(kept, name)
	}
	if len(tags) == 0 {
		return title, []uuid.UUID{}, []string{}
	}
	return strings.TrimRight(title[:max(open-1, 0)], " "), tags, kept
}

// composeTagTitle builds a title from base, the chosen members' names and
// the kept names: base + " [" + chosen, then kept, joined by ", " + "]". With
// neither it is just base, so untagging removes the group entirely.
func composeTagTitle(base string, chosen, kept []string) string {
	names := make([]string, 0, len(chosen)+len(kept))
	names = append(names, chosen...)
	names = append(names, kept...)
	if len(names) == 0 {
		return base
	}
	group := "[" + strings.Join(names, ", ") + "]"
	if base == "" {
		return group
	}
	return base + " " + group
}
