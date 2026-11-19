package app

// DefaultHouseholdName exposes defaultHouseholdName to package app_test. The
// users CHECK caps display names at 50 runes, so its 100-rune cut cannot be
// reached through a real sign-in and is proven on the helper directly.
var DefaultHouseholdName = defaultHouseholdName

// NextScheduleDate exposes nextScheduleDate to package app_test: its calendar
// arithmetic across daylight-saving changes is proven on the function itself.
var NextScheduleDate = nextScheduleDate
