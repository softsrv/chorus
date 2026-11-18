// Canned API answers: Leo with 3 points, history +3 Quest: Make bed, and the 422 body for points.

export const members = [
  { id: 'm-leo', household_id: 'h1', name: 'Leo', has_pin: false, points: 3 },
];

export const pointsHistory = {
  'm-leo': [
    { id: 'pa1', kind: 'quest', reason: 'Quest: Make bed', points: 3, earned_at: '2025-01-01T12:00:00Z', created_at: '2025-01-01T12:00:00Z' },
  ],
};

export const overRemovalError = {
  error: {
    code: 'validation_error',
    message: "Leo only has 3 points — you can't remove 5.",
    fields: {
      points: "Leo only has 3 points — you can't remove 5.",
    },
  },
};
