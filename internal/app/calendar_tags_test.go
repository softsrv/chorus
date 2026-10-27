package app

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/db"
)

// retag is what SetEntryTags does to a title: parse it against the members,
// then compose the base with the chosen members' names and the kept names.
func retag(t *testing.T, title string, members []TagMember, ids []uuid.UUID) string {
	t.Helper()
	names, bad := tagNames(members, ids)
	if len(bad) > 0 {
		t.Fatalf("tagNames(%v): untaggable ids %v", ids, bad)
	}
	base, _, kept := parseTagTitle(title, members)
	return composeTagTitle(base, names, kept)
}

// CLM-4 (and CLM-7's unit case): the tag group is the trailing " [..]" group
// when at least one name in it is a taggable member; other names are kept
// after the chosen members; an empty set leaves the bare base; a title never
// gains a second group; untaggable names are never composed into a title.
func TestTagTitle(t *testing.T) {
	mom, sam, sam2, jo, ann := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	members := tagMembers([]db.ListHouseholdMembersRow{
		{UserID: mom, DisplayName: "Mom"},
		{UserID: sam, DisplayName: " Sam "},
	})
	withUntaggable := tagMembers([]db.ListHouseholdMembersRow{
		{UserID: mom, DisplayName: "Mom"},
		{UserID: sam, DisplayName: "Sam"},
		{UserID: sam2, DisplayName: "sam"},
		{UserID: jo, DisplayName: "Smith, Jo"},
		{UserID: ann, DisplayName: "Ann [twin]"},
	})

	parses := []struct {
		name, title, base string
		tags              []uuid.UUID
		kept              []string
	}{
		{"two members", "Dentist [Mom, Sam]", "Dentist", []uuid.UUID{mom, sam}, []string{}},
		{"trimmed, case-insensitive", "Dentist [mom ]", "Dentist", []uuid.UUID{mom}, []string{}},
		{"member and kept name", "Dentist [Mom, Q3]", "Dentist", []uuid.UUID{mom}, []string{"Q3"}},
		{"no member: group is base", "Meeting [Q3]", "Meeting [Q3]", []uuid.UUID{}, []string{}},
		{"untagged title unchanged", "Dentist", "Dentist", []uuid.UUID{}, []string{}},
		{"not trailing", "[Mom] Dentist", "[Mom] Dentist", []uuid.UUID{}, []string{}},
		{"no space before group", "Dentist[Mom]", "Dentist[Mom]", []uuid.UUID{}, []string{}},
		{"only a group", "[Mom]", "", []uuid.UUID{mom}, []string{}},
		{"last group only", "Meeting [Q3] [Mom]", "Meeting [Q3]", []uuid.UUID{mom}, []string{}},
	}
	for _, tc := range parses {
		t.Run("parse/"+tc.name, func(t *testing.T) {
			base, tags, kept := parseTagTitle(tc.title, members)
			if base != tc.base || !slices.Equal(tags, tc.tags) || !slices.Equal(kept, tc.kept) {
				t.Errorf("parseTagTitle(%q) = %q %v %q, want %q %v %q", tc.title, base, tags, kept, tc.base, tc.tags, tc.kept)
			}
		})
	}

	retags := []struct {
		name, title string
		ids         []uuid.UUID
		want        string
	}{
		{"tag untagged", "Dentist", []uuid.UUID{mom, sam}, "Dentist [Mom, Sam]"},
		{"untag all", "Dentist [Mom, Sam]", nil, "Dentist"},
		{"retag keeps non-member, one group", "Dentist [Mom, Q3]", []uuid.UUID{sam}, "Dentist [Sam, Q3]"},
		{"non-member group stays base", "Meeting [Q3]", []uuid.UUID{mom}, "Meeting [Q3] [Mom]"},
		{"retag replaces", "Dentist [Mom]", []uuid.UUID{sam}, "Dentist [Sam]"},
		{"untag keeps kept name", "Dentist [Mom, Q3]", nil, "Dentist [Q3]"},
		{"tag then untag round trip", "Dentist [Mom, Sam]", []uuid.UUID{}, "Dentist"},
		{"repeat retag is stable", "Dentist [Sam, Q3]", []uuid.UUID{sam}, "Dentist [Sam, Q3]"},
	}
	for _, tc := range retags {
		t.Run("retag/"+tc.name, func(t *testing.T) {
			if got := retag(t, tc.title, members, tc.ids); got != tc.want {
				t.Errorf("retag(%q, %v) = %q, want %q", tc.title, tc.ids, got, tc.want)
			}
		})
	}

	if got := composeTagTitle("Dentist", nil, nil); got != "Dentist" {
		t.Errorf("composeTagTitle(Dentist, none) = %q, want Dentist", got)
	}

	t.Run("taggable classification", func(t *testing.T) {
		want := map[uuid.UUID]string{mom: "", sam: TagReasonDuplicateName, sam2: TagReasonDuplicateName,
			jo: TagReasonReservedChar, ann: TagReasonReservedChar}
		for _, m := range withUntaggable {
			if m.Reason != want[m.UserID] || m.Taggable != (want[m.UserID] == "") {
				t.Errorf("%q: taggable=%v reason=%q, want reason %q", m.Name, m.Taggable, m.Reason, want[m.UserID])
			}
		}
	})

	t.Run("untaggable names never composed", func(t *testing.T) {
		for _, id := range []uuid.UUID{sam, sam2, jo, ann, uuid.New()} {
			if names, bad := tagNames(withUntaggable, []uuid.UUID{mom, id}); len(bad) != 1 || bad[0] != id {
				t.Errorf("tagNames(mom, %v) = %v bad %v, want %v refused", id, names, bad, id)
			}
		}
		// A title naming an untaggable member is not read as tagging them.
		if base, tags, _ := parseTagTitle("Lunch [Smith, Jo]", withUntaggable); base != "Lunch [Smith, Jo]" || len(tags) != 0 {
			t.Errorf("parse 'Lunch [Smith, Jo]' = %q %v, want unchanged, no tags", base, tags)
		}
		if base, tags, _ := parseTagTitle("Lunch [Sam]", withUntaggable); base != "Lunch [Sam]" || len(tags) != 0 {
			t.Errorf("parse 'Lunch [Sam]' with two Sams = %q %v, want unchanged, no tags", base, tags)
		}
	})
}
