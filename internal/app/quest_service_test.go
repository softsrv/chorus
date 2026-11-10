package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsForeignKeyViolation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "exact 23503 PgError",
			err:  &pgconn.PgError{Code: "23503"},
			want: true,
		},
		{
			name: "wrapped 23503 PgError",
			err:  fmt.Errorf("execute claim: %w", &pgconn.PgError{Code: "23503"}),
			want: true,
		},
		{
			name: "unique violation 23505",
			err:  &pgconn.PgError{Code: "23505"},
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("some error"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isForeignKeyViolation(tc.err)
			if got != tc.want {
				t.Errorf("isForeignKeyViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
