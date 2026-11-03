package reader_test

import (
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/reader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShardSpec_Validate(t *testing.T) {
	tests := []struct {
		name    string
		spec    reader.ShardSpec
		wantErr string
	}{
		{"valid single", reader.ShardSpec{Mod: 4, Owned: []int{0}}, ""},
		{"valid multi", reader.ShardSpec{Mod: 4, Owned: []int{0, 2}}, ""},
		{"mod zero", reader.ShardSpec{Mod: 0, Owned: []int{0}}, "Mod must be > 0"},
		{"mod negative", reader.ShardSpec{Mod: -1, Owned: []int{0}}, "Mod must be > 0"},
		{"empty owned", reader.ShardSpec{Mod: 4, Owned: []int{}}, "Owned must be non-empty"},
		{"owned too high", reader.ShardSpec{Mod: 4, Owned: []int{4}}, "out of range"},
		{"owned negative", reader.ShardSpec{Mod: 4, Owned: []int{-1}}, "out of range"},
		{"duplicate", reader.ShardSpec{Mod: 4, Owned: []int{1, 1}}, "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestShardSpec_AppendQuery(t *testing.T) {
	s := reader.ShardSpec{Mod: 4, Owned: []int{0, 2}}

	query := "SELECT * FROM t WHERE gvk = $1 AND txid > $2"
	args := []any{"apps/v1/Deployment", uint64(100)}

	query, args = s.AppendQuery(query, args)

	assert.Contains(t, query, "AND abs(hashtext(namespace)::bigint) % $3 = ANY($4::int[])")
	require.Len(t, args, 4)
	assert.Equal(t, "apps/v1/Deployment", args[0])
	assert.Equal(t, uint64(100), args[1])
	assert.Equal(t, 4, args[2])
	assert.Equal(t, []int{0, 2}, args[3])
}

func TestShardSpec_AppendQuery_DifferentArgCount(t *testing.T) {
	s := reader.ShardSpec{Mod: 2, Owned: []int{1}}

	query := "SELECT * FROM t WHERE a = $1 AND b = $2 AND c = $3 AND d = $4"
	args := []any{"a", "b", "c", "d"}

	query, args = s.AppendQuery(query, args)

	assert.Contains(t, query, "AND abs(hashtext(namespace)::bigint) % $5 = ANY($6::int[])")
	require.Len(t, args, 6)
	assert.Equal(t, 2, args[4])
	assert.Equal(t, []int{1}, args[5])
}

func TestShardSpec_ToListFilter(t *testing.T) {
	s := reader.ShardSpec{Mod: 3, Owned: []int{0}}

	f := s.ToListFilter()

	require.Len(t, f.WhereClauses, 1)
	assert.Equal(t, "abs(hashtext(namespace)::bigint) % $2 = ANY($3::int[])", f.WhereClauses[0])
	require.Len(t, f.WhereArgs, 2)
	assert.Equal(t, 3, f.WhereArgs[0])
	assert.Equal(t, []int{0}, f.WhereArgs[1])
}

func TestShardSpec_ToListFilter_MultiOwned(t *testing.T) {
	s := reader.ShardSpec{Mod: 4, Owned: []int{1, 3}}

	f := s.ToListFilter()

	require.Len(t, f.WhereClauses, 1)
	assert.Equal(t, "abs(hashtext(namespace)::bigint) % $2 = ANY($3::int[])", f.WhereClauses[0])
	require.Len(t, f.WhereArgs, 2)
	assert.Equal(t, 4, f.WhereArgs[0])
	assert.Equal(t, []int{1, 3}, f.WhereArgs[1])
}
