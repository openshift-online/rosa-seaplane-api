package reader

import "fmt"

// ShardSpec restricts a List or Watch to rows whose shard key hash,
// modulo Mod, falls in Owned. Nil *ShardSpec means unsharded.
//
// The shard key is the row's namespace plus its database UID when KeyLabel is
// empty or absent on a row. When KeyLabel is set and present, its value replaces
// the row UID in that namespace-scoped key. This lets owned children share their
// owner's UID while parent and unrelated rows fall back to their own UID. The
// label must remain stable for the lifetime of a row so List and Watch continue
// to agree on shard ownership.
type ShardSpec struct {
	Mod      int    // hash modulus, > 0
	Owned    []int  // residues owned by this replica, each in [0, Mod)
	KeyLabel string // optional label whose value replaces the row UID in the shard key
}

func (s *ShardSpec) Validate() error {
	if s.Mod <= 0 {
		return fmt.Errorf("shard: Mod must be > 0, got %d", s.Mod)
	}
	if len(s.Owned) == 0 {
		return fmt.Errorf("shard: Owned must be non-empty")
	}
	seen := make(map[int]bool, len(s.Owned))
	for _, o := range s.Owned {
		if o < 0 || o >= s.Mod {
			return fmt.Errorf("shard: Owned value %d out of range [0, %d)", o, s.Mod)
		}
		if seen[o] {
			return fmt.Errorf("shard: duplicate Owned value %d", o)
		}
		seen[o] = true
	}
	return nil
}

func (s *ShardSpec) shardClause(startParam int) (string, []any) {
	if s.KeyLabel != "" {
		clause := fmt.Sprintf(
			"abs(hashtext(namespace || '/' || COALESCE(metadata->'labels'->>$%d, uid::text))::bigint) %% $%d = ANY($%d::int[])",
			startParam, startParam+1, startParam+2)
		return clause, []any{s.KeyLabel, s.Mod, s.Owned}
	}
	clause := fmt.Sprintf(
		"abs(hashtext(namespace || '/' || uid::text)::bigint) %% $%d = ANY($%d::int[])",
		startParam, startParam+1)
	return clause, []any{s.Mod, s.Owned}
}

// AppendQuery appends the shard WHERE predicate to query and args.
// Parameter numbering is derived from len(args).
func (s *ShardSpec) AppendQuery(query string, args []any) (string, []any) {
	clause, shardArgs := s.shardClause(len(args) + 1)
	return query + " AND " + clause, append(args, shardArgs...)
}

// ToListFilter returns a ListFilter with the shard predicate for use with List().
func (s *ShardSpec) ToListFilter() *ListFilter {
	clause, args := s.shardClause(2)
	return &ListFilter{
		WhereClauses: []string{clause},
		WhereArgs:    args,
	}
}
