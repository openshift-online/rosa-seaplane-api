package reader

import "fmt"

// ShardSpec restricts a List or Watch to namespaces whose hash,
// modulo Mod, falls in Owned. Nil *ShardSpec means unsharded.
// Shard membership is immutable per object: namespace is part of the
// primary key and never updated in place.
type ShardSpec struct {
	Mod   int   // hash modulus, > 0
	Owned []int // residues owned by this replica, each in [0, Mod)
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
	clause := fmt.Sprintf(
		"abs(hashtext(namespace)::bigint) %% $%d = ANY($%d::int[])",
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
