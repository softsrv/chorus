// Age-range fixtures for the quest and family tests: the JSON shapes the
// server emits (questJSON with min_age, max_age and ineligible_member_ids,
// familyMemberJSON, the admin birthdays read and the error envelope). Which
// members are out of range is the server's answer (ineligible_member_ids);
// the client never works it out.

export const HOUSEHOLD = 'h1';

export const SAM = { id: 'm-sam', household_id: HOUSEHOLD, name: 'Sam', has_pin: false, points: 0 };
export const AVA = { id: 'm-ava', household_id: HOUSEHOLD, name: 'Ava', has_pin: false, points: 0 };
export const LEO = { id: 'm-leo', household_id: HOUSEHOLD, name: 'Leo', has_pin: false, points: 0 };
export const MEMBERS = [SAM, AVA, LEO];

const base = {
  household_id: HOUSEHOLD, library_id: null, points: 5, repeat_days: 1, notes: '', streak_bonus: 0,
  available: true, claimed_by: null, claimed_at: null, awaiting_approval: false, completed_at: null,
  available_at: null, created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
  shareable: false, teamwork_bonus: 0, shared: false, participants: [],
  min_age: null, max_age: null, ineligible_member_ids: [],
};

export const ageQuest = (over) => ({ ...base, ...over });

// The four range shapes: none, both bounds, a minimum only, a maximum only.
export const FEED_CAT = ageQuest({ id: 'q-cat', name: 'Feed the cat', points: 5 });
export const MOW_LAWN = ageQuest({ id: 'q-mow', name: 'Mow the lawn', points: 20, min_age: 10, max_age: 14, ineligible_member_ids: [AVA.id] });
export const READ_CHAPTER = ageQuest({ id: 'q-read', name: 'Read a chapter', points: 10, min_age: 6, ineligible_member_ids: [LEO.id] });
export const WATER_PLANTS = ageQuest({ id: 'q-water', name: 'Water plants', points: 3, max_age: 12, ineligible_member_ids: [AVA.id] });
export const RANGE_QUESTS = [FEED_CAT, MOW_LAWN, READ_CHAPTER, WATER_PLANTS];

// A shareable quest Ava holds although the server lists her as out of range
// (her birthday was set after she accepted it): it stays hers.
export const TIDY_ROOM = ageQuest({
  id: 'q-tidy', name: 'Tidy room', points: 10, available: false, claimed_by: AVA.id,
  claimed_at: '2025-01-02T00:00:00Z', shareable: true, min_age: 14, ineligible_member_ids: [AVA.id],
});

// A quest someone else holds is not noted for Ava, whatever its range.
export const RAKE_LEAVES = ageQuest({
  id: 'q-rake', name: 'Rake leaves', available: false, claimed_by: SAM.id, min_age: 10, max_age: 14,
  ineligible_member_ids: [AVA.id],
});

// The admin birthdays read: GET .../family-members/birthdays. Leo's age is
// deliberately not what his date gives today: the browser shows the server's.
export const BIRTHDAYS = {
  items: [
    { member_id: SAM.id, birthday: '2015-04-02', age: 11 },
    { member_id: AVA.id, birthday: null, age: null },
    { member_id: LEO.id, birthday: '2020-01-01', age: 42 },
  ],
};

export const MIN_ABOVE_MAX = "Minimum age can't be above the maximum age.";

// A 422 on the quest form for a minimum above the maximum.
export const RANGE_422 = [422, { error: {
  code: 'validation_failed', message: 'Check the highlighted fields.', fields: { max_age: MIN_ABOVE_MAX },
} }];
