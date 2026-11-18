// Canned API answers: Maya (42 points) and her history +10 helped grandma,
// -5 left toys out (both admin), +5 Feed the cat, +3 combo.

export const members = [
  { id: 'm-maya', household_id: 'h1', name: 'Maya', has_pin: true, points: 42 },
];

export const pointsHistory = {
  'm-maya': [
    { id: 'pa4', kind: 'admin', reason: 'helped grandma', points: 10, earned_at: '2025-01-04T12:00:00Z', created_at: '2025-01-04T12:00:00Z' },
    { id: 'pa3', kind: 'admin', reason: 'left toys out', points: -5, earned_at: '2025-01-03T12:00:00Z', created_at: '2025-01-03T12:00:00Z' },
    { id: 'pa2', kind: 'quest', reason: 'Quest: Feed the cat', points: 5, earned_at: '2025-01-02T12:00:00Z', created_at: '2025-01-02T12:00:00Z' },
    { id: 'pa1', kind: 'combo', reason: 'Combo bonus', points: 3, earned_at: '2025-01-01T12:00:00Z', created_at: '2025-01-01T12:00:00Z' },
  ],
};
