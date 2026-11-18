// Canned API answers for the Family tab's member view: Maya with 42 points and
// her points history, newest first, in the shapes internal/http/handlers emits
// (familyMemberJSON, the points award in {items: [...]}).

export const members = [
  { id: 'm-maya', household_id: 'h1', name: 'Maya', has_pin: true, points: 42 },
];

export const pointsHistory = {
  'm-maya': [
    { id: 'pa2', kind: 'quest', reason: 'Quest: Feed the cat', points: 5, earned_at: '2025-01-02T12:00:00Z', created_at: '2025-01-02T12:00:00Z' },
    { id: 'pa1', kind: 'combo', reason: 'Combo bonus', points: 3, earned_at: '2025-01-01T12:00:00Z', created_at: '2025-01-01T12:00:00Z' },
  ],
};
